SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/thunter/snowstate/internal/buildinfo.Version=$(VERSION) \
	-X github.com/thunter/snowstate/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/thunter/snowstate/internal/buildinfo.Date=$(DATE)
export CGO_ENABLED ?= 0

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
IMAGE_PLATFORMS := linux/amd64,linux/arm64
# Extra flags for image-multiarch, such as --push and --tag options.
IMAGE_ARGS ?=

.PHONY: all build release image image-multiarch lint test cover e2e integration vuln tools clean foundation-check

all: lint test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/snowstate ./cmd/snowstate

# Cross-compiles every platform into dist/ and writes checksums.txt.
release:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o "dist/snowstate_$${os}_$${arch}$$ext" ./cmd/snowstate || exit 1; \
	done
	cd dist && (command -v sha256sum >/dev/null && sha256sum snowstate_* || shasum -a 256 snowstate_*) > checksums.txt

image:
	docker build -f deploy/docker/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t snowstate:$(VERSION) .

# Needs docker buildx. Without IMAGE_ARGS it only checks that each platform builds.
image-multiarch:
	docker buildx build -f deploy/docker/Dockerfile --platform $(IMAGE_PLATFORMS) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) $(IMAGE_ARGS) .

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

# Checks the Foundation with each pinned Terraform and OpenTofu, no credentials needed.
foundation-check: tools
	./deploy/terraform/check.sh

e2e: tools
	go test -count=1 -timeout 30m ./test/e2e/...

# Skips itself unless SNOWSTATE_IT_* is set.
integration:
	go test -count=1 -timeout 60m -tags integration ./test/integration/...

vuln:
	govulncheck ./...

clean:
	rm -rf bin dist cover.out .tools
