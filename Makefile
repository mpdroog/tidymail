# tidymail
BIN := tidymail
DIR ?= mail.d

.PHONY: all build test lint check run init clean

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/tidymail

test:
	go test ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

# What CI should run.
check: vet lint test

init: build
	./$(BIN) -dir $(DIR) init

run: build
	./$(BIN) -dir $(DIR) serve

sync: build
	./$(BIN) -dir $(DIR) sync

reindex: build
	./$(BIN) -dir $(DIR) reindex

clean:
	rm -f $(BIN)
