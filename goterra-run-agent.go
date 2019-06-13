package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	terraConfig "github.com/osallou/goterra-lib/lib/config"
	terraUser "github.com/osallou/goterra-lib/lib/user"
	"github.com/rs/cors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongo "go.mongodb.org/mongo-driver/mongo"
	mongoOptions "go.mongodb.org/mongo-driver/mongo/options"

	terraToken "github.com/osallou/goterra-lib/lib/token"
	"github.com/streadway/amqp"
)

// Version of server
var Version string

var mongoClient mongo.Client
var nsCollection *mongo.Collection
var runCollection *mongo.Collection
var runOutputsCollection *mongo.Collection

// HomeHandler manages base entrypoint
var HomeHandler = func(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"version": Version, "message": "ok"}
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Run represents a deployment info for an app
type Run struct {
	ID         primitive.ObjectID `json:"id" bson:"_id,omitempty"`
	AppID      string             `json:"appID"` // Application id
	Inputs     map[string]string  `json:"inputs"`
	Status     string             `json:"status"`
	Endpoint   string             `json:"endpoint"`
	Namespace  string             `json:"namespace"`
	UID        string
	Start      int64         `json:"start"`
	Duration   time.Duration `json:"duration"`
	Outputs    string        `json:"outputs"`
	Deployment string        `json:"deployment"`
}

// RunAction is message struct to be sent to the run component
// action: apply or destroy
// id: identifier of the run
type RunAction struct {
	Action string `json:"action"`
	ID     string `json:"id"`
}

// GotAction manage received message
func GotAction(action RunAction) (float64, []byte, error) {
	config := terraConfig.LoadConfig()
	tsStart := time.Now()
	var outputs = make([]byte, 0)

	if action.Action == "deploy" {
		runPathElts := []string{config.Deploy.Path, action.ID}
		runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"init"}
		cmd := exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			fmt.Printf("Terraform init failed: %s\n", tfErr)
			fmt.Printf("[ERROR] %s", cmdOut)
			return 0, cmdOut, tfErr
		}
		fmt.Printf("[Terraform:init]%s", cmdOut)

		cmdName = "terraform"
		cmdArgs = []string{"apply", "-auto-approve"}
		cmd = exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			fmt.Printf("Terraform apply failed: %s\n", tfErr)
			fmt.Printf("[ERROR] %s", cmdOut)
			return 0, cmdOut, tfErr
		}
		fmt.Printf("[Terraform:apply] %s\n", cmdOut)

		cmdName = "terraform"
		cmdArgs = []string{"output", "-json"}
		cmd = exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			fmt.Printf("Terraform output failed: %s\n", tfErr)
			fmt.Printf("[ERROR] %s", cmdOut)
			return 0, cmdOut, tfErr
		}
		fmt.Printf("[Terraform:output] %s\n", cmdOut)
		outputs = cmdOut
	} else if action.Action == "destroy" {
		runPathElts := []string{config.Deploy.Path, action.ID}
		runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"destroy"}
		cmd := exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			fmt.Printf("Terraform destroy failed: %s\n", tfErr)
			fmt.Printf("[ERROR] %s", cmdOut)
			return 0, cmdOut, tfErr
		}
		fmt.Printf("[Terraform:destroy] %s\n", cmdOut)
	}
	tsEnd := time.Now()
	duration := tsEnd.Sub(tsStart).Seconds()
	fmt.Printf("[DEBUG] duration: %f", duration)
	return duration, outputs, nil
}

// GetRunAction gets a message from rabbitmq exchange
func GetRunAction() error {
	config := terraConfig.LoadConfig()
	if config.Amqp == "" {
		fmt.Printf("[ERROR] no amqp defined\n")
		return fmt.Errorf("No AMQP config found")
	}
	conn, err := amqp.Dial(config.Amqp)
	if err != nil {
		fmt.Printf("[ERROR] failed to connect to %s\n", config.Amqp)
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		fmt.Printf("[ERROR] failed to connect to amqp\n")
		return err
	}

	err = ch.ExchangeDeclare(
		"gotrun", // name
		"fanout", // type
		true,     // durable
		false,    // auto-deleted
		false,    // internal
		false,    // no-wait
		nil,      // arguments
	)
	if err != nil {
		fmt.Printf("[ERROR] failed to connect to open exchange\n")
		return err
	}

	queue, queueErr := ch.QueueDeclare(
		"gotaction",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,   // arguments
	)
	if queueErr != nil {
		fmt.Printf("[ERROR] failed to create queue\n")
		return queueErr
	}

	bindErr := ch.QueueBind(queue.Name, "", "gotrun", false, nil)
	if bindErr != nil {
		fmt.Printf("[ERROR] failed to bind queue to exchange\n")
		return bindErr
	}

	msgs, consumeErr := ch.Consume(
		queue.Name, // queue
		"",         // consumer
		false,      // auto-ack
		false,      // exclusive
		false,      // no-local
		false,      // no-wait
		nil,        // args
	)
	if consumeErr != nil {
		fmt.Printf("[ERROR] failed to get messages\n")
		return consumeErr
	}

	// forever := make(chan bool)

	go func() {
		for d := range msgs {
			fmt.Printf("[DEBUG] got a message %s", d.Body)
			action := RunAction{}
			err := json.Unmarshal(d.Body, &action)
			if err != nil {
				fmt.Printf("failed to decode message %s", d.Body)
				d.Ack(true)
				continue
			}
			duration, outputs, msgErr := GotAction(action)
			status := "success"
			if msgErr != nil {
				fmt.Printf("Error with action: %s", msgErr)
				status = "failure"
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				objID, _ := primitive.ObjectIDFromHex(action.ID)
				run := bson.M{
					"_id": objID,
				}
				var outputData map[string]*json.RawMessage
				deployment := ""
				outErr := json.Unmarshal(outputs, &outputData)
				if outErr == nil {
					if val, ok := outputData["deployment_id"]; ok {
						var valData map[string]string
						depErr := json.Unmarshal(*val, &valData)
						if depErr == nil {
							deployment = valData["value"]
						}
					}
				}
				newrun := bson.M{
					"$set": bson.M{
						"duration":   duration,
						"status":     status,
						"outputs":    string(outputs),
						"deployment": deployment,
					},
				}
				runCollection.FindOneAndUpdate(ctx, run, newrun)
				cancel()
			}
			d.Ack(true)
		}
	}()

	// log.Printf(" [*] Waiting for logs. To exit press CTRL+C")
	// <-forever

	return nil
}

// CheckToken checks Fernet token
func CheckToken(authToken string) (user terraUser.User, err error) {
	// config := terraConfig.LoadConfig()

	tokenStr := strings.Replace(authToken, "Bearer", "", -1)
	tokenStr = strings.TrimSpace(tokenStr)

	msg, errMsg := terraToken.FernetDecode([]byte(tokenStr))
	if errMsg != nil {
		return user, errMsg
	}
	json.Unmarshal(msg, &user)
	return user, nil
}

// GetRunStatusHandler returns run info
var GetRunStatusHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]
	runID := vars["run"]

	claims, err := CheckToken(r.Header.Get("Authorization"))
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		respError := map[string]interface{}{"message": fmt.Sprintf("Auth error: %s", err)}
		json.NewEncoder(w).Encode(respError)
		return
	}

	// Get run
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var rundb Run
	objID, _ := primitive.ObjectIDFromHex(runID)
	run := bson.M{
		"_id":       objID,
		"namespace": nsID,
	}
	err = runCollection.FindOne(ctx, run).Decode(&rundb)
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": "failed to create run"}
		json.NewEncoder(w).Encode(respError)
		return
	}

	if !claims.Admin && claims.UID != rundb.UID {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		respError := map[string]interface{}{"message": fmt.Sprintf("Not allowed to access this resource: %s", err)}
		json.NewEncoder(w).Encode(respError)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rundb)

}

// End of Run ************************************

func main() {

	config := terraConfig.LoadConfig()

	consulErr := terraConfig.ConsulDeclare("got-run-agent", "/run-agent")
	if consulErr != nil {
		fmt.Printf("Failed to register: %s", consulErr.Error())
		panic(consulErr)
	}

	mongoClient, err := mongo.NewClient(mongoOptions.Client().ApplyURI(config.Mongo.URL))
	if err != nil {
		log.Printf("[ERROR] Failed to connect to mongo server %s\n", config.Mongo.URL)
		os.Exit(1)
	}
	ctx, cancelMongo := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMongo()

	err = mongoClient.Connect(ctx)
	if err != nil {
		log.Printf("[ERROR] Failed to connect to mongo server %s\n", config.Mongo.URL)
		os.Exit(1)
	}
	nsCollection = mongoClient.Database(config.Mongo.DB).Collection("ns")
	runCollection = mongoClient.Database(config.Mongo.DB).Collection("run")
	runOutputsCollection = mongoClient.Database(config.Mongo.DB).Collection("runoutputs")

	amqpErr := GetRunAction()
	if amqpErr != nil {
		log.Printf("[ERROR] Failed to listen amqp messages: %s", amqpErr)
		os.Exit(1)
	}

	r := mux.NewRouter()
	r.HandleFunc("/run-agent", HomeHandler).Methods("GET")

	r.HandleFunc("/run-agent/ns/{id}/run/{run}", GetRunStatusHandler).Methods("GET") // deploy app

	handler := cors.Default().Handler(r)

	loggedRouter := handlers.LoggingHandler(os.Stdout, handler)

	srv := &http.Server{
		Handler: loggedRouter,
		Addr:    fmt.Sprintf("%s:%d", config.Web.Listen, config.Web.Port),
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())

}
