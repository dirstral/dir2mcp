# Build binaries. Requires Go 1.24+.
.PHONY: build build-dir2mcp
build: build-dir2mcp

DIR2MCP_VERSION ?= 0.0.0-dev
DIR2MCP_LDFLAGS ?= -X dir2mcp/internal/buildinfo.Version=$(DIR2MCP_VERSION)
GOCYCLO_BIN ?= $(shell command -v gocyclo 2>/dev/null || echo "$$(go env GOPATH)/bin/gocyclo")

build-dir2mcp:
	go build -ldflags "$(DIR2MCP_LDFLAGS)" -o dir2mcp ./cmd/dir2mcp/

# Run dir2mcp up (set MISTRAL_API_KEY first)
.PHONY: up
up: build
	./dir2mcp up

.PHONY: all clean clean-all help fmt vet lint cyclo test check ci benchmark inspector-smoke conformance

all: check

help:
	@echo "Targets:"
	@echo "  all       - default target (runs check)"
	@echo "  clean     - remove build artifacts and local test caches only"
	@echo "  clean-all - full clean including Go build cache (use sparingly)"
	@echo "  fmt       - format Go code"
	@echo "  vet    - run go vet"
	@echo "  lint   - run golangci-lint"
	@echo "  cyclo  - run gocyclo -over 15 ./internal/"
	@echo "  test   - run go test"
	@echo "  check  - fmt + vet + lint + cyclo + test + build"
	@echo "  ci     - vet + cyclo + test (CI-safe default)"
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
	"$(GOCYCLO_BIN)" -over 15 ./internal/

test:
	go test ./...

conformance:
	go test ./tests/conformance/...

check: fmt vet lint cyclo test build

ci: vet cyclo test

benchmark:
	# run the large-corpus retrieval benchmark only
	go test -bench BenchmarkSearchBothLargeCorpus -run ^$$ -benchmem ./internal/retrieval

SMOKE_CORPUS ?= tests/testdata/smoke-corpus
inspector-smoke: build
	bash scripts/inspector-smoke.sh ./dir2mcp "$(SMOKE_CORPUS)"

clean:
	rm -f dir2mcp coverage.out
	# only purge the test cache so we don't evict the global build cache
	go clean -testcache >/dev/null 2>&1 || true

clean-all: clean
	# perform a full cache wipe, use only when you really need it
	go clean -cache -testcache >/dev/null 2>&1 || true
