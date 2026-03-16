default:
    @just --list

build:
    go build -o devops .
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags agent -o devops-agent .

build-windows:
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o devops.exe .

build-all: build build-windows

test:
    go test -v -count=1 ./...

test-agent:
    go test -v -count=1 -tags agent ./...

install: build
    cp devops /usr/local/bin/devops

agent-deploy HOST: build
    scp devops-agent root@{{HOST}}:/usr/local/bin/devops.new
    ssh root@{{HOST}} 'chmod +x /usr/local/bin/devops.new && mv /usr/local/bin/devops.new /usr/local/bin/devops && systemctl restart devops-agent'

clean:
    rm -f devops devops-agent
