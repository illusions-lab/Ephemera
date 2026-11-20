.DEFAULT_GOAL := help

CONFIG ?= configs/ephemera.example.yaml
IMAGE_CACHE ?= bin/image
DEBIAN_IMAGE ?=
DEBIAN_SHA512 ?=

export CONFIG IMAGE_CACHE DEBIAN_IMAGE DEBIAN_SHA512

.PHONY: help build build-linux bundle-linux fmt test vet lint-shell check config-check image-prepare vm-boot vm-test vm-debug

help:
	@printf '%s\n' \
	  'make build         Build the CLI and daemon into bin/' \
	  'make build-linux   Cross-build the CLI and daemon for Linux amd64' \
	  'make bundle-linux  Stage a Linux amd64 installer bundle in bin/bundle' \
	  'make fmt           Format Go source' \
	  'make check         Run Go tests, vet, shell checks, and config validation' \
	  'make config-check  Validate the example or pass CONFIG=/path/to/config.yaml' \
	  'make image-prepare Assemble and boot-check the configured runner image' \
	  'make vm-boot       Check Debian boot, SSH, cloud-init, and systemd' \
	  'make vm-test       Run the installer smoke test in a fresh Debian VM' \
	  'make vm-debug      Run the installer smoke test and keep the VM for SSH' \
	  '' \
	  'VM targets require DEBIAN_IMAGE (path or HTTPS URL) and DEBIAN_SHA512.'

build:
	mkdir -p bin
	go build -o bin/ephemera ./cmd/ephemera
	go build -o bin/ephemerad ./cmd/ephemerad

build-linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ephemera-linux-amd64 ./cmd/ephemera
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ephemerad-linux-amd64 ./cmd/ephemerad

bundle-linux: build-linux
	mkdir -p bin/bundle
	cp bin/ephemera-linux-amd64 bin/bundle/ephemera
	cp bin/ephemerad-linux-amd64 bin/bundle/ephemerad
	cp packaging/ephemera.service bin/bundle/ephemera.service
	cp scripts/install.sh bin/bundle/install.sh

fmt:
	go fmt ./...

test:
	go test ./...

vet:
	go vet ./...

lint-shell:
	bash -n scripts/install.sh
	bash -n scripts/dev-vm.sh
	bash -n scripts/release-tag.sh
	bash -n scripts/ci-install-test.sh
	shellcheck scripts/install.sh scripts/dev-vm.sh scripts/release-tag.sh scripts/ci-install-test.sh

config-check:
	go run ./cmd/ephemera config validate --config "$$CONFIG"

check: test vet lint-shell config-check

image-prepare:
	go run ./cmd/ephemera image prepare --config "$$CONFIG" --image-cache "$$IMAGE_CACHE"

vm-boot:
	./scripts/dev-vm.sh --image "$$DEBIAN_IMAGE" --sha512 "$$DEBIAN_SHA512"

vm-test:
	./scripts/dev-vm.sh --image "$$DEBIAN_IMAGE" --sha512 "$$DEBIAN_SHA512" --installer-smoke

vm-debug:
	./scripts/dev-vm.sh --image "$$DEBIAN_IMAGE" --sha512 "$$DEBIAN_SHA512" --installer-smoke --keep
