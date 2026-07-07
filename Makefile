.PHONY: build test lint fmt fmt-check install-hooks run migrate-up migrate-down clean

BINARY := bin/gateway
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/gateway

test:
	$(GO) test -v -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	@if command -v goimports >/dev/null 2>&1; then goimports -w .; fi

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v goimports >/dev/null 2>&1; then \
		out=$$(goimports -l .); if [ -n "$$out" ]; then echo "goimports needed:"; echo "$$out"; exit 1; fi; \
	fi

install-hooks:
	git config core.hooksPath .githooks

run: build
	$(BINARY) serve

migrate-up: build
	$(BINARY) migrate up

migrate-down: build
	$(BINARY) migrate down

clean:
	rm -rf bin/
	$(GO) clean -testcache
