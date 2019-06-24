package main

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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

// Event represent an action (deploy, destroy, etc.) on a run (historical data)
type Event struct {
	TS      int64  `json:"ts"`
	Action  string `json:"action"`
	Success bool   `json:"success"`
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
	Start      int64   `json:"start"`
	Duration   float64 `json:"duration"`
	Outputs    string  `json:"outputs"`
	Deployment string  `json:"deployment"`
	Events     []Event `json:"events"`
}

// RunAction is message struct to be sent to the run component
// action: apply or destroy
// id: identifier of the run
type RunAction struct {
	Action  string            `json:"action"`
	ID      string            `json:"id"`
	Secrets map[string]string `json:"secrets"`
}

// GotState gets the terraform show -json output
func GotState(action RunAction) ([]byte, error) {
	config := terraConfig.LoadConfig()
	var state = make([]byte, 0)

	runPathElts := []string{config.Deploy.Path, action.ID}
	runPath := strings.Join(runPathElts, "/")
	var (
		cmdStateOut []byte
		tfStateErr  error
	)

	cmdName := "terraform"
	cmdArgs := []string{"show", "-json"}
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Dir = runPath
	if cmdStateOut, tfStateErr = cmd.Output(); tfStateErr != nil {
		log.Error().Str("run", action.ID).Str("show", string(cmdStateOut)).Msgf("Terraform show failed: %s", tfStateErr)
		return cmdStateOut, tfStateErr
	}
	log.Info().Str("run", action.ID).Str("show", string(cmdStateOut)).Msg("Terraform:show")
	state = cmdStateOut
	return state, nil
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
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform init failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:init")

		cmdName = "terraform"
		cmdArgs = []string{"apply", "-auto-approve", "-input=false"}
		// Add sensitive data via env vars when executing command

		cmd = exec.Command(cmdName, cmdArgs...)
		cmd.Env = os.Environ()
		for key, val := range action.Secrets {
			log.Info().Str("secret", key).Msg("Received some secrets...")
			if val == "" {
				log.Error().Str("secret", key).Msg("secret is empty")
			}
			cmd.Env = append(cmd.Env, fmt.Sprintf("TF_VAR_%s=%s", key, val))
		}
		cmd.Dir = runPath
		cmdOut, tfErrExec := cmd.CombinedOutput()
		if tfErrExec != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform apply failed: %s", tfErrExec)
			return 0, cmdOut, tfErrExec
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:apply")

		cmdName = "terraform"
		cmdArgs = []string{"output", "-json"}
		cmd = exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform output failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:output")
		outputs = cmdOut

	} else if action.Action == "destroy" {
		runPathElts := []string{config.Deploy.Path, action.ID}
		runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"destroy", "-auto-approve"}
		cmd := exec.Command(cmdName, cmdArgs...)
		cmd.Env = os.Environ()
		for key, val := range action.Secrets {
			log.Info().Str("run", action.ID).Str("secret", key).Msg("Received some secrets...")
			if val == "" {
				log.Error().Str("run", action.ID).Str("secret", key).Msg("secret is empty")
			}
			cmd.Env = append(cmd.Env, fmt.Sprintf("TF_VAR_%s=%s", key, val))
		}
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform destroy failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:destroy")
		outputs = cmdOut
	} else if strings.HasPrefix(action.Action, "state") {
		stateElts := strings.Split(action.Action, ":")
		runPathElts := []string{config.Deploy.Path, action.ID}
		runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"state", "list"}
		if len(stateElts) > 1 {
			cmdArgs = []string{"state", "show", stateElts[1]}
		}
		cmd := exec.Command(cmdName, cmdArgs...)
		cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform state failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:state")
		outputs = cmdOut
	}
	tsEnd := time.Now()
	duration := tsEnd.Sub(tsStart).Seconds()
	log.Info().Str("run", action.ID).Float64("duration", duration).Msg("Terraform:done")
	return duration, outputs, nil
}

// GetRunAction gets a message from rabbitmq exchange
func GetRunAction() error {
	config := terraConfig.LoadConfig()
	if config.Amqp == "" {
		log.Error().Msg("no amqp defined")
		return fmt.Errorf("No AMQP config found")
	}
	conn, err := amqp.Dial(config.Amqp)
	if err != nil {
		log.Error().Msgf("failed to connect to %s", config.Amqp)
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Error().Msg("failed to connect to amqp")
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
		log.Error().Msg("failed to connect to open exchange")
		return err
	}

	queue, queueErr := ch.QueueDeclare(
		"gotaction",
		true,  // durable
		false, // auto-deleted
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if queueErr != nil {
		log.Error().Msg("failed to create queue")
		return queueErr
	}

	bindErr := ch.QueueBind(queue.Name, "", "gotrun", false, nil)
	if bindErr != nil {
		log.Error().Msg("failed to bind queue to exchange")
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
		log.Error().Msg("failed to get messages")
		return consumeErr
	}

	forever := make(chan bool)

	go func() {
		log.Debug().Msgf("listen for messages on %s", queue.Name)
		for d := range msgs {
			log.Debug().Msg("got a message")
			action := RunAction{}
			err := json.Unmarshal(d.Body, &action)
			if err != nil {
				log.Error().Msgf("failed to decode message %s", d.Body)
				d.Ack(true)
				continue
			}
			duration, outputs, msgErr := GotAction(action)
			actionOK := true
			status := fmt.Sprintf("%s_success", action.Action)
			if msgErr != nil {
				log.Error().Str("run", action.ID).Msgf("Error with action: %s", msgErr)
				status = fmt.Sprintf("%s_failure", action.Action)
				actionOK = false
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
						var valData map[string]interface{}
						depErr := json.Unmarshal(*val, &valData)
						if depErr == nil {
							deployment = valData["value"].(string)
						}
					}
				} else {
					log.Error().Str("run", action.ID).Msgf("Failed to decode json ouputs of terraform %s", outputs)
				}

				state, stateErr := GotState(action)
				if stateErr != nil {
					state = []byte("")
				}

				newrun := bson.M{
					"$set": bson.M{
						"duration":   duration,
						"status":     status,
						"outputs":    string(outputs),
						"deployment": deployment,
						"state":      string(state),
					},
					"$push": bson.M{
						"events": bson.M{
							"ts":      time.Now().Unix(),
							"action":  action.Action,
							"success": actionOK,
						},
					},
				}
				updatedRun := Run{}
				upErr := runCollection.FindOneAndUpdate(ctx, run, newrun).Decode(&updatedRun)
				if upErr != nil {
					log.Error().Str("run", action.ID).Msgf("Failed to update run: %s", upErr)
				}
				cancel()
			}
			d.Ack(true)
		}
	}()

	<-forever

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

// GetRunStatesHandler gets run terraform states
var GetRunStatesHandler = func(w http.ResponseWriter, r *http.Request) {
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

	action := RunAction{Action: "state", ID: runID}
	_, output, err := GotAction(action)
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": fmt.Sprintf("failed to get state: %s", err)}
		json.NewEncoder(w).Encode(respError)
		return
	}
	states := strings.Split(string(output), "\n")
	w.Header().Add("Content-Type", "application/json")
	resp := map[string]interface{}{"states": states}
	json.NewEncoder(w).Encode(resp)
}

// GetRunStateHandler gets run terraform states
var GetRunStateHandler = func(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nsID := vars["id"]
	runID := vars["run"]
	stateID := vars["state"]

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

	action := RunAction{Action: fmt.Sprintf("state:%s", stateID), ID: runID}
	_, output, err := GotAction(action)
	if err != nil {
		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		respError := map[string]interface{}{"message": fmt.Sprintf("failed to get state: %s", err)}
		json.NewEncoder(w).Encode(respError)
		return
	}
	w.Header().Add("Content-Type", "application/json")
	resp := map[string]interface{}{"state": string(output), "id": stateID}
	json.NewEncoder(w).Encode(resp)
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

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if os.Getenv("GOT_DEBUG") != "" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	config := terraConfig.LoadConfig()

	consulErr := terraConfig.ConsulDeclare("got-run-agent", "/run-agent")
	if consulErr != nil {
		log.Error().Msgf("Failed to register: %s", consulErr.Error())
		panic(consulErr)
	}

	mongoClient, err := mongo.NewClient(mongoOptions.Client().ApplyURI(config.Mongo.URL))
	if err != nil {
		log.Error().Msgf("Failed to connect to mongo server %s", config.Mongo.URL)
		os.Exit(1)
	}
	ctx, cancelMongo := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMongo()

	err = mongoClient.Connect(ctx)
	if err != nil {
		log.Error().Msgf("Failed to connect to mongo server %s", config.Mongo.URL)
		os.Exit(1)
	}
	nsCollection = mongoClient.Database(config.Mongo.DB).Collection("ns")
	runCollection = mongoClient.Database(config.Mongo.DB).Collection("run")
	runOutputsCollection = mongoClient.Database(config.Mongo.DB).Collection("runoutputs")

	go GetRunAction()
	/*
		if amqpErr != nil {
			log.Printf("[ERROR] Failed to listen amqp messages: %s", amqpErr)
			os.Exit(1)
		}*/

	r := mux.NewRouter()
	r.HandleFunc("/run-agent", HomeHandler).Methods("GET")

	r.HandleFunc("/run-agent/ns/{id}/run/{run}", GetRunStatusHandler).Methods("GET")              // deploy app
	r.HandleFunc("/run-agent/ns/{id}/run/{run}/state", GetRunStatesHandler).Methods("GET")        // deploy app
	r.HandleFunc("/run-agent/ns/{id}/run/{run}/state/{state}", GetRunStateHandler).Methods("GET") // deploy app

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		AllowedHeaders:   []string{"Authorization"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
	})
	handler := c.Handler(r)

	loggedRouter := handlers.LoggingHandler(os.Stdout, handler)

	srv := &http.Server{
		Handler: loggedRouter,
		Addr:    fmt.Sprintf("%s:%d", config.Web.Listen, config.Web.Port),
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	srv.ListenAndServe()

}
