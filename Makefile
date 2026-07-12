GO      ?= go
BIN_DIR ?= bin
GOOS_TARGET ?= linux

.PHONY: all build build-host lint fmt vet test clean web

all: lint build test

# Rebuild the management console into pkg/webui/dist (committed, so plain
# `go build` always embeds a working console without needing node).
web:
	cd web && npm ci && npm run build && npm test

# Cross-compile all Linux binaries (works from macOS; CI runs natively).
build:
	mkdir -p $(BIN_DIR)
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/uffd-handler ./cmd/uffd-handler
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/probe-server ./test/probe/server
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/probe-client ./test/probe/client
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/uffdiocopy ./test/bench/uffdiocopy
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/genreport ./test/bench/report
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/apiserver ./cmd/apiserver
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/nodeagent ./cmd/nodeagent
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/guestd ./cmd/guestd
	GOOS=$(GOOS_TARGET) CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/embervm ./cmd/embervm

lint: fmt vet
	shellcheck scripts/*.sh test/integration/*.sh test/bench/*.sh deploy/singlenode/*.sh

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	GOOS=$(GOOS_TARGET) $(GO) vet ./...

test:
	$(GO) test ./pkg/... ./test/...

clean:
	rm -rf $(BIN_DIR) work results
