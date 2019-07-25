# Goterra run agent

Microservice in charge of executing terraform recipes for goterra

## Executing in Docker

Terraform commands can be executed in a side container to isolate commands and files (templates, variables, etc..)

To do so,  env vars must be set:

* GOT_DOCKER_USE: set to 1 if you wish to run commands in a side container
* GOT_DOCKER_IMAGE: osallou/goterra-run-executor
* GOT_DOCKER_DIR: base host path where goterra files are located

If goterra-run-agent is itself in a container, directory  /var/run/docker.sock must be mounted in container to allow docker commands:

    docker ... -v  /var/run/docker.sock:/var/run/docker.sock  goterra-run-agent

Image GOT_DOCKER_IMAGE is built from Dockerfile-run-executor file
