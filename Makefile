VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/atlas

# Pure Go: no cgo anywhere (SQLite is modernc.org/sqlite).
export CGO_ENABLED := 0

.PHONY: all build test lint fmt tidy clean

all: fmt lint test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/atlas

test:
	go test -timeout 5m ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
