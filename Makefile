BINARY   := wiznote-migrate
MODULE   := github.com/jackchen701/wiznote-migrate
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build install clean tidy lint

all: build

## build: compile binary to ./bin/wiznote-migrate
build:
	go build $(LDFLAGS) -o bin/$(BINARY) .

## install: install binary to $GOPATH/bin
install:
	go install $(LDFLAGS) .

## tidy: tidy go modules
tidy:
	go mod tidy

## lint: run go vet
lint:
	go vet ./...

## clean: remove build artifacts
clean:
	rm -rf bin/

## help: show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
