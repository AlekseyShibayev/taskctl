GO ?= go
BIN_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell (git describe --tags --always --dirty 2>/dev/null || echo dev) | sed 's/^v//')

.PHONY: build install test

build:
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/taskctl ./cmd/taskctl

install: build
	@mkdir -p $(BIN_DIR)
	install -m 0755 bin/taskctl $(BIN_DIR)/taskctl
	@echo "Installed: $(BIN_DIR)/taskctl"
	@echo "  MCP client: command $(BIN_DIR)/taskctl, args [mcp], transport stdio"

test:
	$(GO) test ./...
