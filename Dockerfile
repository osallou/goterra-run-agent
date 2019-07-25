FROM golang:1.11

LABEL maintainer="Olivier Sallou <olivier.sallou@irisa.fr>"

WORKDIR /root
# Install Docker client
RUN wget https://download.docker.com/linux/static/stable/x86_64/docker-18.06.3-ce.tgz
RUN tar xvfz docker-18.06.3-ce.tgz

# Set the Current Working Directory inside the container
WORKDIR $GOPATH/src/github.com/osallou/goterra-run-agent

RUN wget https://releases.hashicorp.com/terraform/0.12.2/terraform_0.12.2_linux_amd64.zip
RUN apt-get update && apt-get install -y unzip
RUN unzip terraform_0.12.2_linux_amd64.zip
RUN VERSION=`curl --silent "https://api.github.com/repos/osallou/terraform-provider-goterra/releases/latest" | grep '"tag_name":'| sed -E 's/.*"([^"]+)".*/\1/'` && curl -L -o terraform-provider-goterra https://github.com/osallou/terraform-provider-goterra/releases/download/$VERSION/terraform-provider-goterra.linux.amd64 && chmod +x terraform-provider-goterra


# Copy everything from the current directory to the PWD(Present Working Directory) inside the container
COPY . .
RUN go get -u github.com/golang/dep/cmd/dep
#RUN go get -d -v ./...
RUN dep ensure

# Install the package
RUN go build -ldflags "-X  main.Version=`git rev-parse --short HEAD`" goterra-run-agent.go
RUN cp goterra-run-agent.yml.example goterra.yml




# Get terraform
# Get terraform-provider-goterra

FROM alpine:latest  
RUN apk --no-cache add ca-certificates
WORKDIR /root/
RUN mkdir -p /root/.terraform.d/plugins
COPY --from=0 /root/docker/docker /usr/bin/
COPY --from=0 /go/src/github.com/osallou/goterra-run-agent/terraform /usr/bin/
COPY --from=0 /go/src/github.com/osallou/goterra-run-agent/terraform-provider-goterra /root/.terraform.d/plugins/ 
RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2
COPY --from=0 /go/src/github.com/osallou/goterra-run-agent/goterra-run-agent .
COPY --from=0 /go/src/github.com/osallou/goterra-run-agent/goterra.yml .
CMD ["./goterra-run-agent"]
