BINARY := workiva-mcp
VERSION ?= dev
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test lint run fmt

build:
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/workiva-mcp

test:
	go test ./...

lint:
	go vet ./...
	@gofmt -l . | grep . && echo "gofmt: files need formatting" && exit 1 || echo "gofmt: clean"

run:
	go run $(LDFLAGS) ./cmd/workiva-mcp

fmt:
	gofmt -w .
