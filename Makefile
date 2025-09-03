# Makefile for building and testing sub-libraries B, C, D

.PHONY: all build test clean

all: build

build: build-memiavl build-store build-versiondb

build-memiavl:
	cd memiavl && go build ./...

build-store:
	cd store && go build ./...

build-versiondb:
	cd versiondb && go build ./...


test: test-memiavl test-store test-versiondb

test-memiavl:
	cd memiavl && go test ./...

test-store:
	cd store && go test ./...

test-versiondb:
	cd versiondb && go test ./...

clean: clean-memiavl clean-store clean-versiondb

clean-memiavl:
	cd memiavl && go clean

clean-store:
	cd store && go clean

clean-versiondb:
	cd versiondb && go clean

fmt:
	go fmt ./memiavl/... ./store/... ./versiondb/...

vet:
	go vet ./memiavl/... ./store/... ./versiondb/...