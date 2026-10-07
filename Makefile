COVERAGE ?= coverage.txt
GO ?= go
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif
BUF_VERSION ?= v1.41.0
BUF_BIN ?= $(GOBIN)/buf
PROTO_DIR ?= proto
BUF_BREAKING_AGAINST ?= origin/main

BUF := $(shell command -v buf 2>/dev/null)
ifeq ($(strip $(BUF)),)
BUF := $(BUF_BIN)
endif

.PHONY: all build test clean fmt vet \
	proto-lint proto-gen proto-gen-ci proto-check-breaking buf-install

all: build

build: build-memiavl build-store build-versiondb

build-memiavl:
	@cd memiavl && $(GO) build -mod=readonly -tags=objstore ./...

build-store:
	@cd store && $(GO) build -mod=readonly -tags=objstore ./...

build-versiondb:
	@cd versiondb && $(GO) build -mod=readonly -tags=objstore ./...


test: test-memiavl test-store test-versiondb

test-memiavl:
	@cd memiavl && $(GO) test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

test-store:
	@cd store && $(GO) test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

test-versiondb:
	@cd versiondb && $(GO) test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

clean: clean-memiavl clean-store clean-versiondb

clean-memiavl:
	@cd memiavl && go clean

clean-store:
	@cd store && go clean

clean-versiondb:
	@cd versiondb && $(GO) clean

fmt:
	$(GO) fmt ./memiavl/... ./store/... ./versiondb/...

vet:
	$(GO) vet ./memiavl/... ./store/... ./versiondb/...

buf-install:
	@if ! command -v buf >/dev/null 2>&1 && [ ! -x "$(BUF_BIN)" ]; then \
		echo "Installing buf $(BUF_VERSION)"; \
		GOBIN=$(GOBIN) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION); \
	fi

proto-lint: buf-install
	$(BUF) lint $(PROTO_DIR) --config $(PROTO_DIR)/buf.yaml

proto-gen: buf-install
	$(BUF) generate $(PROTO_DIR) --template $(PROTO_DIR)/buf.gen.yaml

proto-gen-ci: proto-gen

proto-check-breaking: buf-install
	$(BUF) breaking $(PROTO_DIR) --against ".git#branch=$(BUF_BREAKING_AGAINST),subdir=$(PROTO_DIR)" --config $(PROTO_DIR)/buf.yaml
