SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/thunter/snowstate/internal/buildinfo.Version=$(VERSION) \
	-X github.com/thunter/snowstate/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/thunter/snowstate/internal/buildinfo.Date=$(DATE)
export CGO_ENABLED ?= 0

.PHONY: all build lint test cover e2e integration vuln tools clean

all: lint test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/snowstate ./cmd/snowstate

lint:
	golangci-lint run ./...

test:
	go test ./cmd/... ./internal/... ./pkg/...

cover:
	go test -coverprofile=cover.out ./cmd/... ./internal/... ./pkg/...
	go tool cover -func=cover.out | tail -1

# Downloads pinned Terraform and OpenTofu binaries into .tools (see test/e2e/tools.sh).
tools:
	./test/e2e/tools.sh

e2e: tools
	go test -count=1 -timeout 30m ./test/e2e/...

# Skips itself unless SNOWSTATE_IT_* is set.
integration:
	go test -count=1 -timeout 60m -tags integration ./test/integration/...

vuln:
	govulncheck ./...

clean:
	rm -rf bin dist cover.out .tools
