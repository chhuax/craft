.PHONY: build test lint fmt tidy release-snapshot

build:
	go build -o bin/craft$(shell go env GOEXE) ./cmd/craft

test:
	go test -count=1 ./...

lint:
	golangci-lint run ./...
	golangci-lint fmt --diff

fmt:
	go fmt ./...

tidy:
	go mod tidy

release-snapshot:
	goreleaser release --snapshot --clean
