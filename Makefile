.PHONY: build test test-integration rootfs demo clean

build:
	go build -o bin/mkube ./cmd/mkube
	go build -o bin/minictl ./cmd/minictl

test:
	go test -race ./...

rootfs:
	./scripts/make-rootfs.sh rootfs

# Needs root: starts real containers.
test-integration: rootfs
	MKUBE_ROOTFS=$(CURDIR)/rootfs go test -count=1 ./...

# Needs root: a 3-node cluster on this machine.
demo:
	./scripts/demo.sh

clean:
	rm -rf bin rootfs
