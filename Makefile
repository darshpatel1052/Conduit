BINARY  := bin/conduit
PKG     := ./cmd/gateway
GO_PKGS := ./cmd/... ./internal/... ./scripts/load/mockprovider/...
GO_FILES := $(shell find cmd internal scripts/load/mockprovider -type f -name '*.go')
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F: '{printf "  \033[36m%-16s\033[0m%s\n", $$1, $$2}'

## build: compile the gateway binary
build:
	go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

## validate: parse and validate the local process and routing configuration
validate:
	go run -buildvcs=false $(PKG) validate

## run: run the gateway against configs/
run:
	go run -buildvcs=false $(PKG)

## test: run the full suite with the race detector
test:
	go test $(GO_PKGS) -race

## test-short: run tests without the race detector (faster)
test-short:
	go test $(GO_PKGS)

## cover: produce a coverage report
cover:
	go test $(GO_PKGS) -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1
	@echo "html report: go tool cover -html=coverage.out"

## vet: run go vet
vet:
	go vet $(GO_PKGS)

## fmt: format all Go source
fmt:
	gofmt -l -w $(GO_FILES)

## check: formatting + vet + race tests, the pre-commit gate
check: fmt vet test

## up: build and start the complete local product stack
up:
	docker compose up --build -d

## demo: run the deterministic project/profile/key/provider-switch story
demo:
	./scripts/demo.sh

## down: stop the local product stack
down:
	docker compose down

## logs: follow all stack logs
logs:
	docker compose logs -f

## clean: remove build artefacts
clean:
	rm -rf bin coverage.out

## verify: run the deterministic story against the real local stores
verify:
	docker compose config --quiet
	./scripts/demo.sh
	docker compose ps

## e2e: end-to-end verification against live Ollama (needs a running gateway)
e2e:
	./scripts/e2e.sh

## image: build the gateway container image
image:
	docker buildx build --load --build-arg VERSION=$(VERSION) -t conduit:$(VERSION) -t conduit:latest .

## image-web: build the dashboard container image
image-web:
	docker buildx build --load -t conduit-dashboard:$(VERSION) -t conduit-dashboard:latest ./web

## load-overhead: measure the gateway's own latency cost against live Ollama
load-overhead:
	k6 run scripts/load/gateway-overhead.js

## load-throughput: measure gateway throughput against the mock upstream
load-throughput:
	go build -buildvcs=false -o bin/mockprovider ./scripts/load/mockprovider
	@echo "start these in separate shells, then re-run:"
	@echo "  ./bin/mockprovider -addr :19100"
	@echo "  ./bin/conduit -config scripts/load/conduit.load.yaml"
	PEAK_RATE=$${PEAK_RATE:-3000} k6 run scripts/load/throughput.js

.PHONY: help build validate run test test-short cover vet fmt check up down logs clean \
        verify e2e image image-web load-overhead load-throughput
