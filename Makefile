.PHONY: check fmt-check test vet build deps

BUILD_OUTPUT ?= .local/bin/orchestrator-mcp
MCP_BUILD_OUTPUT ?= .local/bin/orchestrator-mcp-server

export GOENV := off
export GOTOOLCHAIN := local
export GOPROXY := off
export GOSUMDB := off
export GOFLAGS := -mod=readonly
export GOCACHE := $(CURDIR)/.local/go-build
export GOPATH := $(CURDIR)/.local/go
export TMPDIR := $(CURDIR)/.local/tmp
export TEST_TELEMETRY_DIR := $(CURDIR)/.local/telemetry

check: fmt-check test vet build

# Explicit, task-local fetch of the locked SDK and its required dependencies.
deps: .local/tmp .local/telemetry/mode
	GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org go mod download

.local/tmp:
	mkdir -p "$@"

.local/telemetry/mode:
	mkdir -p "$(@D)"
	printf 'off\n' > "$@"

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

test: .local/tmp .local/telemetry/mode
	go test ./...

vet: .local/tmp .local/telemetry/mode
	go vet ./...

build: .local/tmp .local/telemetry/mode
	mkdir -p "$(dir $(BUILD_OUTPUT))"
	CGO_ENABLED=0 go build -trimpath -o "$(BUILD_OUTPUT)" ./cmd/orchestrator-mcp
	mkdir -p "$(dir $(MCP_BUILD_OUTPUT))"
	CGO_ENABLED=0 go build -trimpath -o "$(MCP_BUILD_OUTPUT)" ./cmd/orchestrator-mcp-server
