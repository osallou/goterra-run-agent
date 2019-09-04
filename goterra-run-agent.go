package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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

	terraModel "github.com/osallou/goterra-lib/lib/model"
)

// Version of server
var Version string

// DEPLOY is to start a new deployment
const DEPLOY string = "deploy"

// DESTROY is to destroy/delete a deployment
const DESTROY string = "destroy"

var mongoClient mongo.Client
var nsCollection *mongo.Collection
var runCollection *mongo.Collection
var runStateCollection *mongo.Collection

// HomeHandler manages base entrypoint
var HomeHandler = func(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"version": Version, "message": "ok"}
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// removeContents delete all files and dirs in *dir*
func removeContents(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return err
	}
	for _, file := range files {
		err = os.RemoveAll(file)
		if err != nil {
			return err
		}
	}
	return nil
}

// GotState gets the terraform show -json output
func GotState(action terraModel.RunAction) ([]byte, error) {
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

func getCommand(cmdName string, cmdArgs []string, cmdSecrets map[string]string, runID string) (*exec.Cmd, error) {

	config := terraConfig.LoadConfig()
	runPathElts := []string{config.Deploy.Path, runID}
	runPath := strings.Join(runPathElts, "/")

	if os.Getenv("GOT_DOCKER_USE") != "1" {
		cmd := exec.Command(cmdName, cmdArgs...)
		cmd.Env = os.Environ()
		if cmdSecrets != nil {
			for key, val := range cmdSecrets {
				log.Info().Str("secret", key).Msg("Received some secrets...")
				if val == "" {
					log.Error().Str("secret", key).Msg("secret is empty")
				}
				cmd.Env = append(cmd.Env, fmt.Sprintf("TF_VAR_%s=%s", key, val))
			}
		}
		cmd.Dir = runPath
		return cmd, nil
	}
	if os.Getenv("GOT_DOCKER_DIR") == "" {
		return nil, fmt.Errorf("Missing env var GOT_DOCKER_DIR")
	}

	localPathElts := []string{os.Getenv("GOT_DOCKER_DIR"), runID}
	mountPath := strings.Join(localPathElts, "/")

	if os.Getenv("GOT_DOCKER_IMAGE") == "" {
		return nil, fmt.Errorf("Missing env var GOT_DOCKER_IMAGE")
	}

	newName := "docker"
	newArgs := []string{"run", "--rm", "-w", runPath, "-v", fmt.Sprintf("%s:%s", mountPath, runPath)}
	newEnv := os.Environ()
	if cmdSecrets != nil {
		for key, val := range cmdSecrets {
			newArgs = append(newArgs, "-e")
			newArgs = append(newArgs, fmt.Sprintf("TF_VAR_%s=%s", key, val))
			// newEnv = append(newEnv, fmt.Sprintf("TF_VAR_%s=%q", key, val))
		}
	}
	newArgs = append(newArgs, os.Getenv("GOT_DOCKER_IMAGE"))
	newArgs = append(newArgs, cmdName)
	newArgs = append(newArgs, cmdArgs...)

	// log.Debug().Msgf("Command:execute:%s %s", newName, strings.Join(newArgs, " "))
	cmd := exec.Command(newName, newArgs...)
	cmd.Env = newEnv
	// cmd.Dir = runPath
	return cmd, nil
}

// GotAction manage received message
func GotAction(action terraModel.RunAction) (float64, []byte, error) {
	config := terraConfig.LoadConfig()
	tsStart := time.Now()
	var outputs = make([]byte, 0)

	if action.Action == DEPLOY {
		log.Info().Str("run", action.ID).Msg("Terraform:request:deploy")
		// runPathElts := []string{config.Deploy.Path, action.ID}
		// runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"init"}

		cmd, cmdErr := getCommand(cmdName, cmdArgs, action.Secrets, action.ID)
		if cmdErr != nil {
			log.Error().Str("run", action.ID).Msgf("Terraform cmd generation failed: %s", cmdErr)
			return 0, []byte("Error"), cmdErr
		}

		//cmd := exec.Command(cmdName, cmdArgs...)
		//cmd.Dir = runPath
		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform init failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:init")

		cmdName = "terraform"
		cmdArgs = []string{"apply", "-auto-approve", "-input=false"}
		// Add sensitive data via env vars when executing command

		/*
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
		*/

		cmd, cmdErr = getCommand(cmdName, cmdArgs, action.Secrets, action.ID)
		if cmdErr != nil {
			log.Error().Str("run", action.ID).Msgf("Terraform cmd generation failed: %s", cmdErr)
			return 0, []byte("Error"), cmdErr
		}

		cmdOut, tfErrExec := cmd.CombinedOutput()
		if tfErrExec != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform apply failed: %s", tfErrExec)
			return 0, cmdOut, tfErrExec
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:apply")

		cmdName = "terraform"
		cmdArgs = []string{"output", "-json"}
		/*
			cmd = exec.Command(cmdName, cmdArgs...)
			cmd.Dir = runPath
		*/

		cmd, cmdErr = getCommand(cmdName, cmdArgs, action.Secrets, action.ID)
		if cmdErr != nil {
			log.Error().Str("run", action.ID).Msgf("Terraform cmd generation failed: %s", cmdErr)
			return 0, []byte("Error"), cmdErr
		}

		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform output failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:output")
		outputs = cmdOut

	} else if action.Action == DESTROY {
		log.Info().Str("run", action.ID).Msg("Terraform:request:destroy")
		runPathElts := []string{config.Deploy.Path, action.ID}
		runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"destroy", "-auto-approve"}
		/*
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
		*/

		cmd, cmdErr := getCommand(cmdName, cmdArgs, action.Secrets, action.ID)
		if cmdErr != nil {
			log.Error().Str("run", action.ID).Msgf("Terraform cmd generation failed: %s", cmdErr)
			return 0, []byte("Error"), cmdErr
		}

		if cmdOut, tfErr = cmd.Output(); tfErr != nil {
			log.Error().Str("run", action.ID).Str("out", string(cmdOut)).Msgf("Terraform destroy failed: %s", tfErr)
			return 0, cmdOut, tfErr
		}
		deleteErr := removeContents(runPath)
		if deleteErr != nil {
			log.Error().Str("run", action.ID).Str("path", runPath).Msg("Terraform:destroy:failed to delete job directory")
		}
		log.Info().Str("run", action.ID).Str("out", string(cmdOut)).Msg("Terraform:destroy")
		outputs = cmdOut
	} else if strings.HasPrefix(action.Action, "state") {
		stateElts := strings.Split(action.Action, ":")
		// runPathElts := []string{config.Deploy.Path, action.ID}
		// runPath := strings.Join(runPathElts, "/")
		var (
			cmdOut []byte
			tfErr  error
		)
		cmdName := "terraform"
		cmdArgs := []string{"state", "list"}
		if len(stateElts) > 1 {
			cmdArgs = []string{"state", "show", stateElts[1]}
		}

		/*
			cmd := exec.Command(cmdName, cmdArgs...)
			cmd.Dir = runPath
		*/

		cmd, cmdErr := getCommand(cmdName, cmdArgs, action.Secrets, action.ID)
		if cmdErr != nil {
			log.Error().Str("run", action.ID).Msgf("Terraform cmd generation failed: %s", cmdErr)
			return 0, []byte("Error"), cmdErr
		}

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

func setStatus(id string, status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objID, _ := primitive.ObjectIDFromHex(id)
	run := bson.M{
		"_id": objID,
	}
	newrun := bson.M{
		"$set": bson.M{
			"status": status,
		},
	}

	updatedRun := terraModel.Run{}
	upErr := runCollection.FindOneAndUpdate(ctx, run, newrun).Decode(&updatedRun)
	if upErr != nil {
		log.Error().Str("run", id).Msgf("Failed to update run: %s", upErr)
		return upErr
	}
	return nil
}

func setState(id string, state []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := bson.M{
		"run":   id,
		"state": string(state),
		"ts":    time.Now().Unix(),
	}

	newRunState, upErr := runStateCollection.InsertOne(ctx, run)
	if upErr != nil {
		log.Error().Str("run", id).Msgf("Failed to insert run state: %s", upErr)
		return upErr
	}
	log.Debug().Str("RunState", newRunState.InsertedID.(primitive.ObjectID).Hex()).Msg("New run state inserted")
	return nil
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

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func(connection *amqp.Connection, channel *amqp.Channel) {
		sig := <-sigs
		channel.Close()
		connection.Close()
		log.Warn().Msgf("Closing AMQP channel and connection after signal %s", sig.String())
		log.Warn().Msg("Ready for shutdown")
	}(conn, ch)

	forever := make(chan bool)

	go func() {
		log.Debug().Msgf("listen for messages on %s", queue.Name)
		for d := range msgs {
			log.Debug().Msg("got a message")
			action := terraModel.RunAction{}
			err := json.Unmarshal(d.Body, &action)
			if err != nil {
				log.Error().Msgf("failed to decode message %s", d.Body)
				d.Ack(true)
				continue
			}
			tmpStatus := fmt.Sprintf("%s_in_progress", action.Action)
			setStatus(action.ID, tmpStatus)

			duration, outputs, msgErr := GotAction(action)
			actionOK := true
			status := fmt.Sprintf("%s_success", action.Action)

			var newrun bson.M
			if msgErr != nil {
				log.Error().Str("run", action.ID).Msgf("Error with action: %s", msgErr)
				status = fmt.Sprintf("%s_failure", action.Action)
				actionOK = false

				newrun = bson.M{
					"$set": bson.M{
						"duration": duration,
						"status":   status,
						"error":    string(outputs),
					},
					"$push": bson.M{
						"events": bson.M{
							"ts":      time.Now().Unix(),
							"action":  action.Action,
							"success": actionOK,
						},
					},
				}

			} else {
				deployment := ""
				errorMsg := ""
				if action.Action == DESTROY {
					// Text only message
					errorMsg = string(outputs)
					outputs = []byte("")
				} else {
					var outputData map[string]*json.RawMessage
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
				}

				state := []byte("")
				var stateErr error
				if action.Action == DEPLOY && actionOK {
					state, stateErr = GotState(action)
					if stateErr != nil {
						state = []byte("")
					}
					setState(action.ID, state)
				}

				var end int64
				if action.Action == DESTROY && actionOK {
					end = time.Now().Unix()
				}

				newrun = bson.M{
					"$set": bson.M{
						"duration":   duration,
						"status":     status,
						"outputs":    string(outputs),
						"error":      errorMsg,
						"deployment": deployment,
						"end":        end,
					},
					"$push": bson.M{
						"events": bson.M{
							"ts":      time.Now().Unix(),
							"action":  action.Action,
							"success": actionOK,
						},
					},
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			objID, _ := primitive.ObjectIDFromHex(action.ID)
			run := bson.M{
				"_id": objID,
			}

			updatedRun := terraModel.Run{}
			upErr := runCollection.FindOneAndUpdate(ctx, run, newrun).Decode(&updatedRun)
			if upErr != nil {
				log.Error().Str("run", action.ID).Msgf("Failed to update run: %s", upErr)
			}
			cancel()

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

	var rundb terraModel.Run
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

	action := terraModel.RunAction{Action: "state", ID: runID}
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

	var rundb terraModel.Run
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

	action := terraModel.RunAction{Action: fmt.Sprintf("state:%s", stateID), ID: runID}
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

	var rundb terraModel.Run
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
	runStateCollection = mongoClient.Database(config.Mongo.DB).Collection("runstate")

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
