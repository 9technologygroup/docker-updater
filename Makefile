BINARY    := dup
AGENT     := dup-agent
PKG       := ./cmd/dup
AGENT_PKG := ./cmd/dup-agent

VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE  := github.com/PatchMon/docker-updater

LDFLAGS := -ldflags "-s -w \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
	-X '$(MODULE)/internal/version.Date=$(DATE)'"

DIST := dist

.PHONY: all build check fmt fmt-check vet lint test test-coverage clean crosscheck snapshot tidy help

all: check build ## Run every check, then build

build: ## Build both binaries with version metadata
	go build -trimpath $(LDFLAGS) -o $(BINARY) $(PKG)
	go build -trimpath $(LDFLAGS) -o $(AGENT) $(AGENT_PKG)

check: fmt-check vet lint test ## fmt, vet, lint and race tests

fmt: ## Format all Go source
	gofmt -s -w .

fmt-check: ## Fail if anything needs formatting
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt -s needed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	go vet ./...

lint: ## golangci-lint, if installed
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed, skipping"; fi

test: ## Run the test suite with the race detector
	go test ./... -race -count=1

test-coverage: ## Tests plus an HTML coverage report
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -html=coverage.out -o coverage.html

tidy: ## Tidy go.mod/go.sum
	go mod tidy

build-linux: ## Cross-build both binaries into dist/, named the way install.sh expects
	@mkdir -p $(DIST)
	@for arch in amd64 arm64 arm 386; do \
		out="$$arch"; [ "$$arch" = arm ] && out=armv7; \
		printf '  linux/%-8s' "$$arch"; \
		GOOS=linux GOARCH=$$arch GOARM=7 CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o $(DIST)/$(BINARY)-linux-$$out $(PKG) && \
		GOOS=linux GOARCH=$$arch GOARM=7 CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o $(DIST)/$(AGENT)-linux-$$out $(AGENT_PKG) && \
		echo "ok" || { echo "FAILED"; exit 1; }; \
	done

crosscheck: ## Compile every release target, proving a tag will build
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo "goreleaser is required so this checks the same targets a release builds."; \
		echo "install it with: go install github.com/goreleaser/goreleaser/v2@latest"; \
		exit 1; \
	}
	goreleaser build --snapshot --clean

snapshot: ## Full release dry run into dist/
	goreleaser release --snapshot --clean --skip=publish

clean: ## Remove build output
	rm -rf $(BINARY) $(AGENT) $(DIST) coverage.out coverage.html

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
