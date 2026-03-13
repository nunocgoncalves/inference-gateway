.PHONY: build test lint run migrate-up migrate-down clean

BINARY := bin/gateway
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/gateway

test:
	$(GO) test -v -race -count=1 ./...

lint:
	golangci-lint run ./...

run: build
	$(BINARY) serve

migrate-up: build
	$(BINARY) migrate up

migrate-down: build
	$(BINARY) migrate down

clean:
	rm -rf bin/
	$(GO) clean -testcache
