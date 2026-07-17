MODULE  := git.dpemmons.com/dpemmons/cswap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)

.PHONY: help build install test race vet fmt lint clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

build: ## Build ./cswap with embedded version
	go build -ldflags "$(LDFLAGS)" -o cswap ./cmd/cswap

install: ## go install with embedded version
	go install -ldflags "$(LDFLAGS)" ./cmd/cswap

test: ## Run all tests
	go test ./...

race: ## Run all tests with the race detector
	go test -race ./...

vet: ## go vet all packages
	go vet ./...

fmt: ## gofmt all source (fails if anything was unformatted)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; gofmt -w .; exit 1; fi

clean: ## Remove built binary
	rm -f cswap
