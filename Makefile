GO ?= go

# trading-agent-go build targets. Linux-first; Windows still uses build.ps1.
.PHONY: build server test e2e clean

# Build every command (desktop + server + CLI).
build:
	$(GO) build ./...

# Build the server edition binary into ./dist.
server:
	$(GO) build -o dist/trading-agent-server ./cmd/trading-agent-server

# Full offline test suite.
test:
	$(GO) test ./...

# P2-2 smoke: build the server binary, run the offline end-to-end test
# (real listener + real HTTP client, stub market data, no real network),
# then the full suite. No LLM key and TA_ALLOW_LIVE stay unset by default.
e2e: server
	$(GO) test ./internal/webui/ -run TestServerEditionE2ESmoke -v
	$(GO) test ./...

clean:
	rm -rf dist
