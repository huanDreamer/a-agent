# huan-agent Makefile
# Common operations: make build / test / lint / run / fmt / tidy / clean

BIN_DIR    := bin
BIN_NAME   := huan-agent
CMD_PKG    := ./cmd/huan-agent

# launchd agent (macOS equivalent of a systemd unit) — see deploy/launchd/README.md
AGENT_LABEL  := com.huan-agent.admin
AGENT_DOMAIN := gui/$(shell id -u)
AGENT_TMPL   := deploy/launchd/$(AGENT_LABEL).plist.tmpl
AGENT_PLIST  := $(HOME)/Library/LaunchAgents/$(AGENT_LABEL).plist
LDFLAGS    := -s -w \
              -X github.com/huan/huan-agent/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
              -X github.com/huan/huan-agent/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
              -X github.com/huan/huan-agent/internal/version.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: all build web test test-race lint fmt tidy run run-admin clean version help \
        agent-install agent-uninstall agent-restart agent-status agent-logs

all: build

## web: Build the admin web UI into internal/server/webui/dist (embedded by the binary)
web:
	cd web && npm install --no-audit --no-fund && npm run build
	@echo "Built admin UI into internal/server/webui/dist"

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

## run-admin: Build and run the admin server (usage API, metrics, web UI)
run-admin: build
	./$(BIN_DIR)/$(BIN_NAME) admin serve --config ./configs/config.yaml

## agent-install: Build and install the launchd agent (start at login, restart on crash)
agent-install: build
	@mkdir -p logs $(HOME)/Library/LaunchAgents
	@sed -e 's|__PROJECT_DIR__|$(CURDIR)|g' -e 's|__HOME_DIR__|$(HOME)|g' \
	    $(AGENT_TMPL) > $(AGENT_PLIST)
	@plutil -lint $(AGENT_PLIST)
	-@launchctl bootout $(AGENT_DOMAIN)/$(AGENT_LABEL) 2>/dev/null || true
	@launchctl bootstrap $(AGENT_DOMAIN) $(AGENT_PLIST)
	@echo "installed $(AGENT_LABEL) -> $(AGENT_PLIST)"
	@echo "logs: make agent-logs    status: make agent-status"

## agent-uninstall: Stop and remove the launchd agent
agent-uninstall:
	-@launchctl bootout $(AGENT_DOMAIN)/$(AGENT_LABEL) 2>/dev/null || true
	@rm -f $(AGENT_PLIST)
	@echo "removed $(AGENT_LABEL)"

## agent-restart: Restart the agent, picking up a rebuilt binary
agent-restart:
	@launchctl kickstart -k $(AGENT_DOMAIN)/$(AGENT_LABEL)

## agent-status: Show the agent's launchd state (pid, last exit, logs)
agent-status:
	@if launchctl print $(AGENT_DOMAIN)/$(AGENT_LABEL) >/dev/null 2>&1; then \
	  launchctl print $(AGENT_DOMAIN)/$(AGENT_LABEL) | sed -n '1,25p'; \
	else \
	  echo "$(AGENT_LABEL) is not installed (run: make agent-install)"; \
	fi

## agent-logs: Follow the agent's stdout/stderr logs
agent-logs:
	@tail -n 50 -f logs/admin.log logs/admin.err.log

## version: Print the version
version: build
	./$(BIN_DIR)/$(BIN_NAME) version

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)
	rm -rf web/node_modules

## help: Show this help
help:
	@echo "Available targets:"
	@grep -E '^##' Makefile | sed 's/^## //'
