COVERAGE ?= coverage.txt

.PHONY: all build test clean

all: build

build: build-memiavl build-store build-versiondb

build-memiavl:
	@cd memiavl && go build -mod=readonly -tags=objstore ./...

build-store:
	@cd store && go build -mod=readonly -tags=objstore ./...

build-versiondb:
	@cd versiondb && go build -mod=readonly -tags=objstore ./...


test: test-memiavl test-store test-versiondb

test-memiavl:
	@cd memiavl && go test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

test-store:
	@cd store && go test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

test-versiondb:
	@cd versiondb && go test -tags=objstore -v -mod=readonly ./... -coverprofile=$(COVERAGE) -covermode=atomic;

clean: clean-memiavl clean-store clean-versiondb

clean-memiavl:
	@cd memiavl && go clean

clean-store:
	@cd store && go clean

clean-versiondb:
	@cd versiondb && go clean

fmt:
	go fmt ./memiavl/... ./store/... ./versiondb/...

vet:
	go vet ./memiavl/... ./store/... ./versiondb/...