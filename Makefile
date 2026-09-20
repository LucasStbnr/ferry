# Ferry development tasks.
#
# `make check` is what CI runs; run it before pushing.

BINARY  := ferry
MODULE  := github.com/LucasStbnr/ferry
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/cli.version=$(VERSION) \
	-X $(MODULE)/internal/cli.commit=$(COMMIT) \
	-X $(MODULE)/internal/cli.date=$(DATE)

.PHONY: all
all: build

.PHONY: build
build: ## Build the ferry binary
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/ferry

.PHONY: install
install: ## Install ferry into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/ferry

.PHONY: test
test: ## Run the tests
	go test ./...

.PHONY: test-race
test-race: ## Run the tests under the race detector
	go test -race ./...

.PHONY: test-short
test-short: ## Run the tests, skipping the end-to-end suite
	go test -short ./...

.PHONY: cover
cover: ## Run the tests and open the coverage report
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format the source
	gofmt -w .
	go run golang.org/x/tools/cmd/goimports@latest -w -local $(MODULE) .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: vet lint test-race ## Everything CI runs

.PHONY: docker
docker: ## Build the container image
	docker build -t ghcr.io/lucasstbnr/ferry:$(VERSION) .

.PHONY: snapshot
snapshot: ## Build release artifacts locally without publishing
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BINARY) dist coverage.out

.PHONY: help
help: ## List the targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'
