default:
    @just --list

build:
    go build -o devops .
    GOOS=linux GOARCH=amd64 go build -tags agent -o devops-agent .

test:
    go test -v -count=1 ./...

test-agent:
    go test -v -count=1 -tags agent ./...

install: build
    cp devops /usr/local/bin/devops

clean:
    rm -f devops devops-agent
