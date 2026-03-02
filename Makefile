# Build binaries. Requires Go 1.24+.
.PHONY: build build-dir2mcp build-dirstral
build: build-dir2mcp build-dirstral

DIR2MCP_VERSION ?= 0.0.0-dev
DIR2MCP_LDFLAGS ?= -X dir2mcp/internal/buildinfo.Version=$(DIR2MCP_VERSION)

build-dir2mcp:
	go build -ldflags "$(DIR2MCP_LDFLAGS)" -o dir2mcp ./cmd/dir2mcp/

build-dirstral:
	go build -o dirstral ./cmd/dirstral/

# Run dir2mcp up (set MISTRAL_API_KEY first)
.PHONY: up
up: build
	./dir2mcp up

.PHONY: all clean clean-all help fmt vet lint test smoke-dirstral check ci benchmark

all: check

help:
	@echo "Targets:"
	@echo "  all       - default target (runs check)"
	@echo "  clean     - remove build artifacts and local test caches only"
	@echo "  clean-all - full clean including Go build cache (use sparingly)"
	@echo "  fmt       - format Go code"
	@echo "  vet    - run go vet"
	@echo "  lint   - run golangci-lint"
	@echo "  test   - run go test"
	@echo "  smoke-dirstral - run dirstral smoke tests"
	@echo "  check  - fmt + vet + lint + test + build"
	@echo "  ci     - vet + test (CI-safe default)"
	@echo "  benchmark - run the large-corpus retrieval benchmark"

fmt:
	gofmt -w $$(find cmd internal tests -name '*.go')

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is required. Install: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run

test:
	go test ./...

smoke-dirstral:
	go test -v -count=1 ./tests/dirstral -run '^TestSmoke'

check: fmt vet lint test build

ci: vet test

benchmark:
	# run the large-corpus retrieval benchmark only
	go test -bench BenchmarkSearchBothLargeCorpus -run ^$$ -benchmem ./internal/retrieval

clean:
	rm -f dir2mcp dirstral coverage.out
	# only purge the test cache so we don't evict the global build cache
	go clean -testcache >/dev/null 2>&1 || true

clean-all: clean
	# perform a full cache wipe, use only when you really need it
	go clean -cache -testcache >/dev/null 2>&1 || true
