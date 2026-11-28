.PHONY: build test lint fmt tidy release-snapshot plugin

VERSION ?= 0.0.0-dev

GOEXE := $(shell go env GOEXE)
GITHUB_DIR := extensions/github
ifeq ($(GOEXE),.exe)
# Windows 宿主按 manifest 的完整文件名启动；平台产物写在 bin 下，保留源码中的通用示例。
GITHUB_DIR := bin/extensions/github
endif

build:
	go build -o bin/craft$(GOEXE) ./cmd/craft
	go -C extensions/github build -o "$(abspath $(GITHUB_DIR))/craft-ext-github$(GOEXE)" .
ifeq ($(GOEXE),.exe)
	sed 's|"./craft-ext-github"|"./craft-ext-github.exe"|' extensions/github/manifest.json > $(GITHUB_DIR)/manifest.json
endif

test:
	go test -count=1 ./...
	go -C extensions/github test -count=1 ./...

lint:
	golangci-lint run ./...
	golangci-lint fmt --diff
	cd extensions/github && golangci-lint run && golangci-lint fmt --diff

fmt:
	go fmt ./...
	go -C extensions/github fmt ./...

tidy:
	go mod tidy
	go -C extensions/github mod tidy

release-snapshot:
	goreleaser release --snapshot --clean

plugin:
	go run ./tools/package --version "$(VERSION)" --out dist/plugin
