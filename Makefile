APP_VERSION ?= $(shell git describe --abbrev=5 --dirty --tags --always)
GIT_COMMIT := $(shell git rev-parse --short=8 HEAD)
BUILD_TIME := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

BINDIR := $(PWD)/bin
OUTPUT_DIR := $(PWD)/_output

GOOS ?= $(shell uname -s | tr '[:upper:]' '[:lower:]')
GOARCH ?= amd64

LDFLAGS := $(LDFLAGS) -X github.com/bougou/go-ipmi/cmd/goipmi/commands.Version=$(APP_VERSION)
LDFLAGS := $(LDFLAGS) -X github.com/bougou/go-ipmi/cmd/goipmi/commands.Commit=$(GIT_COMMIT)
LDFLAGS := $(LDFLAGS) -X github.com/bougou/go-ipmi/cmd/goipmi/commands.BuildAt=$(BUILD_TIME)

PATH := $(BINDIR):$(PATH)
SHELL := env PATH='$(PATH)' /bin/sh

all: build

# Run tests
test: fmt vet
	@# Disable --race until https://github.com/kubernetes-sigs/controller-runtime/issues/1171 is fixed.
	ginkgo --randomizeAllSpecs --randomizeSuites --failOnPending --flakeAttempts=2 \
			--cover --coverprofile cover.out --trace --progress  $(TEST_ARGS)\
			./pkg/... ./cmd/...

# Build goipmi binary
build: fmt vet
	go build -ldflags "$(LDFLAGS)" -o $(OUTPUT_DIR)/goipmi ./cmd/goipmi

# Cross compiler
build-all: fmt vet
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -a -o $(OUTPUT_DIR)/goipmi-$(APP_VERSION)-linux-amd64 ./cmd/goipmi
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -a -o $(OUTPUT_DIR)/goipmi-$(APP_VERSION)-linux-arm64 ./cmd/goipmi
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -a -o $(OUTPUT_DIR)/goipmi-$(APP_VERSION)-darwin-amd64 ./cmd/goipmi
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -a -o $(OUTPUT_DIR)/goipmi-$(APP_VERSION)-darwin-arm64 ./cmd/goipmi

# Run go fmt against code
fmt:
	go fmt ./...

# Run go vet against code
vet:
	go vet ./...

lint:
	$(BINDIR)/golangci-lint run --timeout 2m0s ./...

dependencies:
	test -d $(BINDIR) || mkdir $(BINDIR)
	GOBIN=$(BINDIR) go install github.com/onsi/ginkgo/ginkgo@v1.16.4

	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s -- -b $(BINDIR) latest

# ---------------------------------------------------------------------------
# E2E tests
# ---------------------------------------------------------------------------
#   make test-e2e-client          — validate goipmi as a client
#   make test-e2e-server          — validate goipmi-server against ipmitool
#   make test-e2e                 — run both
#
# Local development loop:
#   make test-e2e-setup           — start ipmi-simulator container
#   make test-e2e-client          — run client tests
#   make test-e2e-server          — run server tests
#   make test-e2e-cleanup         — stop ipmi-simulator container
#
# The test scripts auto-start/stop the simulator when Docker is available,
# so setup/cleanup are optional conveniences.

IPMI_SIM_IMAGE ?= vaporio/ipmi-simulator:master
SIM_CONTAINER := goipmi-e2e-sim

test-e2e-setup:
	@if docker ps -a --format '{{.Names}}' | grep -q '^${SIM_CONTAINER}$$'; then \
		echo "Simulator container already exists."; \
	elif ss -uln | grep -q ':623 '; then \
		echo "Port 623 is already in use, skipping simulator."; \
	else \
		docker run -d --name ${SIM_CONTAINER} -p 623:623/udp ${IPMI_SIM_IMAGE}; \
		echo "Simulator started."; \
	fi

test-e2e-cleanup:
	docker rm -f ${SIM_CONTAINER} 2>/dev/null || true

test-e2e-client: build
	./test/e2e/client_test.sh

test-e2e-server:
	./test/e2e/server_test.sh

test-e2e: test-e2e-client test-e2e-server
