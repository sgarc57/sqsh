MODULE     := sqsh
VERSION    ?= dev
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X '$(MODULE)/internal/version.Version=$(VERSION)' \
           -X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
           -X '$(MODULE)/internal/version.BuildDate=$(BUILD_DATE)'

BIN_DIR := bin

.PHONY: build test lint integration localstack-up localstack-down integration-real-aws clean

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/sqshd ./cmd/sqshd
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/sqsh ./cmd/sqsh

# Unit tests only. Anything requiring LocalStack lives behind the
# `integration` build tag and is exercised by the `integration` target.
test:
	go test ./...

lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; ran go vet only"; \
	fi

# Pinned: newer LocalStack images may require LOCALSTACK_AUTH_TOKEN, which is
# passed through when set.
LOCALSTACK_IMAGE := localstack/localstack:4.9
LOCALSTACK_NAME  := sqsh-localstack

# Requires Docker. Brings up LocalStack, runs tests tagged `integration`,
# tears LocalStack down regardless of test outcome.
integration: localstack-up
	go test -tags=integration -count=1 -timeout=10m ./test/integration/...; \
		status=$$?; $(MAKE) --no-print-directory localstack-down; exit $$status

localstack-up:
	docker run -d --rm --name $(LOCALSTACK_NAME) -p 127.0.0.1:4566:4566 \
		-e SERVICES=sns,sqs,dynamodb,sts -e LOCALSTACK_AUTH_TOKEN \
		$(LOCALSTACK_IMAGE) >/dev/null
	@for i in $$(seq 1 60); do \
		curl -sf http://127.0.0.1:4566/_localstack/health >/dev/null && exit 0; sleep 1; \
	done; echo "LocalStack did not become healthy"; exit 1

localstack-down:
	-docker rm -f $(LOCALSTACK_NAME) >/dev/null 2>&1

# Task 9.7: the isolation test against a REAL AWS account, using the default
# credential chain. Creates and deletes sqsh-it-* resources in SQSH_IT_REGION.
integration-real-aws:
	SQSH_IT_REAL_AWS=1 go test -tags=integration -count=1 -timeout=15m -v -run 'TestIsolation' ./test/integration/...

clean:
	rm -rf $(BIN_DIR)
