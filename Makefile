.PHONY: build test lint

BINARY_NAME ?= websocket-channel

build:
	go build -o $(BINARY_NAME) ./cmd/websocket-channel
	@echo "Built: $(BINARY_NAME)"

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run
