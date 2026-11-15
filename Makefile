GO ?= go
BIN_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test cover vet fmt clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/taskctl ./cmd/taskctl

install: build
	@mkdir -p $(BIN_DIR)
	install -m 0755 bin/taskctl $(BIN_DIR)/taskctl
	@echo
	@echo "Installed: $(BIN_DIR)/taskctl"
	@echo "Connect it to OpenCode:"
	@echo "  opencode mcp add taskctl --global -- $(BIN_DIR)/taskctl mcp"
	@echo "Check:"
	@echo "  $(BIN_DIR)/taskctl doctor"

test:
	$(GO) test ./...

cover:
	$(GO) test -cover ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
