default:
    @just --list

build:
    go build -o devops .

test:
    go test -v -count=1 ./...

install: build
    cp devops /usr/local/bin/devops
    codesign -s - /usr/local/bin/devops

clean:
    rm -f devops

