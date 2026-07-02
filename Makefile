# Build binaries. Requires Go 1.24+.
.PHONY: build build-dir2mcp build-elevenlabs-bridge
build: build-dir2mcp

DIR2MCP_VERSION ?= 0.0.0-dev
DIR2MCP_LDFLAGS ?= -X github.com/dirstral/dir2mcp/internal/buildinfo.Version=$(DIR2MCP_VERSION)
GOBIN_DIR := $(shell sh -c 'gobin=$$(go env GOBIN); if [ -n "$$gobin" ]; then echo "$$gobin"; else echo "$$(go env GOPATH | cut -d: -f1)/bin"; fi')
ifndef GOCYCLO_BIN
GOCYCLO_BIN := $(shell command -v gocyclo 2>/dev/null || echo "$(GOBIN_DIR)/gocyclo")
endif
ifndef INEFFASSIGN_BIN
INEFFASSIGN_BIN := $(shell command -v ineffassign 2>/dev/null || echo "$(GOBIN_DIR)/ineffassign")
endif
ifndef MISSPELL_BIN
MISSPELL_BIN := $(shell command -v misspell 2>/dev/null || echo "$(GOBIN_DIR)/misspell")
endif

build-dir2mcp:
	go build -ldflags "$(DIR2MCP_LDFLAGS)" -o dir2mcp ./cmd/dir2mcp/

build-elevenlabs-bridge:
	go build -o elevenlabs-bridge ./cmd/elevenlabs-bridge/

# Run dir2mcp up (set MISTRAL_API_KEY first)
.PHONY: up
up: build
	./dir2mcp up

.PHONY: all clean clean-all help fmt vet lint cyclo ineffassign misspell test test-race check ci benchmark inspector-smoke conformance

all: check

help:
	@echo "Targets:"
	@echo "  all       - default target (runs check)"
	@echo "  clean     - remove build artifacts and local test caches only"
	@echo "  clean-all - full clean including Go build cache (use sparingly)"
	@echo "  fmt       - format Go code"
	@echo "  vet    - run go vet"
	@echo "  lint   - run golangci-lint"
	@echo "  cyclo  - run gocyclo -over 15 over the whole tree"
	@echo "  ineffassign - run ineffassign over the whole tree"
	@echo "  misspell    - run misspell over the whole tree"
	@echo "  test   - run go test"
	@echo "  test-race - run go test -race on the concurrency-sensitive packages (needs CGO)"
	@echo "  check  - fmt + vet + lint + cyclo + ineffassign + misspell + test + build"
	@echo "  ci     - vet + cyclo + ineffassign + misspell + test (CI-safe default)"
	@echo "  build-elevenlabs-bridge - build the ElevenLabs webhook bridge binary"
	@echo "  conformance      - run black-box conformance tests (tests/conformance/)"
	@echo "  benchmark        - run the large-corpus retrieval benchmark"
	@echo "  inspector-smoke  - build and run MCP inspector headless smoke test"

fmt:
	gofmt -w $$(find cmd internal tests -name '*.go')

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is required. Install: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run

cyclo:
	@test -x "$(GOCYCLO_BIN)" || (echo "gocyclo is required. Install: go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0" && exit 1)
	"$(GOCYCLO_BIN)" -over 15 cmd internal tests

ineffassign:
	@test -x "$(INEFFASSIGN_BIN)" || (echo "ineffassign is required. Install: go install github.com/gordonklaus/ineffassign@latest" && exit 1)
	"$(INEFFASSIGN_BIN)" ./...

misspell:
	@test -x "$(MISSPELL_BIN)" || (echo "misspell is required. Install: go install github.com/client9/misspell/cmd/misspell@latest" && exit 1)
	"$(MISSPELL_BIN)" -error cmd internal tests docs README.md

test:
	go test ./...

# test-race exercises the goroutine-heavy embed/index/watch/store paths under the
# race detector so concurrency regressions (issue #419) are caught. It needs the
# C toolchain (CGO_ENABLED=1); it is a separate target rather than folded into
# `check` because `check`/`ci` run with CGO_ENABLED=0.
test-race:
	CGO_ENABLED=1 go test -race ./internal/cli/... ./internal/index/... ./tests/cli ./tests/index ./tests/store

test-release-tools:
	cd scripts && python3 -m unittest discover -p 'test_*.py'

conformance:
	go test ./tests/conformance/...

check: fmt vet lint cyclo ineffassign misspell test test-release-tools build

ci: vet cyclo ineffassign misspell test test-release-tools

benchmark:
	# run the large-corpus retrieval benchmark only
	go test -bench BenchmarkSearchBothLargeCorpus -run ^$$ -benchmem ./internal/retrieval

SMOKE_CORPUS ?= tests/testdata/smoke-corpus
inspector-smoke: build
	bash scripts/inspector-smoke.sh ./dir2mcp "$(SMOKE_CORPUS)"

# Pre-release end-to-end gate: speaks MCP to a RUNNING dir2mcp daemon and
# exercises ask/search/open_file (the retrieval surface that broke across
# v0.9.x). Point it at a freshly (re)indexed corpus on the candidate binary
# BEFORE tagging a release; all checks must pass. Needs a live daemon + the
# corpus's provider credentials, so it is a manual gate, not part of `make ci`.
#   make release-smoke STATE_DIR=~/Downloads/stas-legal/.dir2mcp
# TRANSPORT=http (default) hits the daemon directly; TRANSPORT=stdio drives the
# real `bunx mcp-remote` bridge Claude Desktop uses (catches client-layer bugs).
# Run both before a release.
STATE_DIR ?= .dir2mcp
TRANSPORT ?= http
release-smoke:
	python3 scripts/release_smoke.py --state-dir "$(STATE_DIR)" --transport "$(TRANSPORT)"

clean:
	rm -f dir2mcp coverage.out
	# only purge the test cache so we don't evict the global build cache
	go clean -testcache >/dev/null 2>&1 || true

clean-all: clean
	# perform a full cache wipe, use only when you really need it
	go clean -cache -testcache >/dev/null 2>&1 || true
