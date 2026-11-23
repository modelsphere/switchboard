IMG ?= switchboard:dev
DOCKER ?= docker

.PHONY: help
help: ## List targets.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/switchboard.
	go build -o bin/switchboard ./cmd/switchboard

.PHONY: test
test: ## Unit tests.
	go test ./...

.PHONY: vet
vet: ## go vet.
	go vet ./...

.PHONY: fmt-check
fmt-check: ## Fail when gofmt would change a file.
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: check
check: fmt-check vet test ## Everything CI runs.

.PHONY: docker-build
docker-build: ## Build the image ($(IMG)).
	$(DOCKER) build -t $(IMG) .
