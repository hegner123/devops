default:
    @just --list

build:
    go build -o devops .
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o devops-linux-amd64 .

test:
    go test -v -count=1 ./...

install: build
    cp devops /usr/local/bin/devops
    codesign -s - /usr/local/bin/devops

agent-deploy HOST: build
    scp devops-linux-amd64 root@{{HOST}}:/usr/local/bin/devops.new
    ssh root@{{HOST}} 'chmod +x /usr/local/bin/devops.new && mv /usr/local/bin/devops.new /usr/local/bin/devops && systemctl restart devops-agent'

clean:
    rm -f devops devops-linux-amd64
