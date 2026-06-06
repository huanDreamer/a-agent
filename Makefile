# huan-agent Makefile
# Common operations: make build / test / lint / run / fmt / tidy / clean

BIN_DIR    := bin
BIN_NAME   := huan-agent
CMD_PKG    := ./cmd/huan-agent
LDFLAGS    := -s -w \
              -X github.com/huan/huan-agent/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
              -X github.com/huan/huan-agent/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
              -X github.com/huan/huan-agent/internal/version.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: all build test test-race lint fmt tidy run clean version help

all: build

## build: Compile the binary to $(BIN_DIR)/$(BIN_NAME)
build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/$(BIN_NAME) $(CMD_PKG)
	@echo "Built $(BIN_DIR)/$(BIN_NAME)"

## test: Run all unit tests
test:
	go test ./...

## test-race: Run all unit tests with race detector
test-race:
	go test -race ./...

## lint: Run golangci-lint (requires golangci-lint installed)
lint:
	golangci-lint run ./...

## fmt: Format all Go files
fmt:
	go fmt ./...

## tidy: Tidy go.mod and go.sum
tidy:
	go mod tidy

## run: Build and run with the default config path
run: build
	./$(BIN_DIR)/$(BIN_NAME) serve --config ./configs/config.example.yaml

## version: Print the version
version: build
	./$(BIN_DIR)/$(BIN_NAME) version

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## help: Show this help
help:
	@echo "Available targets:"
	@grep -E '^##' Makefile | sed 's/^## //'
