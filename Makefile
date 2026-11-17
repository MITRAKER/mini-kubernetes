.PHONY: build test test-integration lint fmt vuln cover check tools rootfs demo clean

GOBIN := $(shell go env GOPATH)/bin

build:
	go build -o bin/mkube ./cmd/mkube
	go build -o bin/minictl ./cmd/minictl

test:
	go test -race ./...

# Needs root: starts real containers.
test-integration: rootfs
	MKUBE_ROOTFS=$(CURDIR)/rootfs go test -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

vuln:
	govulncheck ./...

# Needs root for a meaningful number: most of the runtime only runs as root.
cover: rootfs
	MKUBE_ROOTFS=$(CURDIR)/rootfs go test -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	./scripts/check-coverage.sh coverage.out 65

# What CI runs on every pull request, minus the root-only jobs.
check: lint test
	go mod tidy -diff

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
	go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
	command -v pre-commit >/dev/null && pre-commit install || echo "pre-commit not found: pip install pre-commit"

rootfs:
	./scripts/make-rootfs.sh rootfs

# Needs root: a 3-node cluster on this machine.
demo:
	./scripts/demo.sh

clean:
	rm -rf bin rootfs coverage.out
