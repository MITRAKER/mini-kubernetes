.PHONY: build test test-integration rootfs clean

build:
	go build -o bin/mkube ./cmd/mkube

test:
	go test ./...

rootfs:
	./scripts/make-rootfs.sh rootfs

test-integration: rootfs
	MKUBE_ROOTFS=$(CURDIR)/rootfs go test -v ./...

clean:
	rm -rf bin rootfs
