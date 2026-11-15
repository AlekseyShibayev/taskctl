GO ?= go
BIN_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell (git describe --tags --always --dirty 2>/dev/null || echo dev) | sed 's/^v//')
LDFLAGS := -s -w -X main.version=$(VERSION)
COVERAGE_MIN ?= 70

.PHONY: build install test cover cover-check vet fmt fmt-check lint tidy-check check clean

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
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

cover-check:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	echo "coverage: $$total% (minimum $(COVERAGE_MIN)%)"; \
	awk -v total="$$total" -v min="$(COVERAGE_MIN)" 'BEGIN { exit (total + 0 < min + 0) }' || { \
		echo "coverage $$total% is below $(COVERAGE_MIN)%"; exit 1; }

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

lint:
	golangci-lint run

tidy-check:
	$(GO) mod tidy
	git diff --exit-code go.mod go.sum

# One target for a pull request: format, vet, lint, tests and coverage gate.
check: fmt-check vet lint cover-check

clean:
	rm -rf bin dist coverage.out
