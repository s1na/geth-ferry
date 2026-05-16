# Set VERSION on the command line for a release build:
#   make build VERSION=0.2.0
# Otherwise we derive a useful default from the current git state.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint fmt clean

build:
	go build -ldflags '$(LDFLAGS)' -o ferry ./cmd/ferry

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -f ferry
