# Build binaries. Requires Go 1.25+ (matches the `go` directive in go.mod).
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

.PHONY: all clean clean-all help fmt fmt-check vet lint cyclo ineffassign misspell test test-race test-release-tools test-annotator check ci benchmark inspector-smoke conformance

all: check

help:
	@echo "Targets:"
	@echo "  all       - default target (runs check)"
	@echo "  clean     - remove build artifacts and local test caches only"
	@echo "  clean-all - full clean including Go build cache (use sparingly)"
	@echo "  fmt       - format Go code in place (developer convenience)"
	@echo "  fmt-check - fail when Go code is unformatted (read-only gate)"
	@echo "  vet    - run go vet"
	@echo "  lint   - run golangci-lint"
	@echo "  cyclo  - run gocyclo -over 15 over the whole tree"
	@echo "  ineffassign - run ineffassign over the whole tree"
	@echo "  misspell    - run misspell over the whole tree"
	@echo "  test   - run go test"
	@echo "  test-race - run go test -race on the concurrency-sensitive packages (needs CGO)"
	@echo "  test-annotator - run the Python annotator suite in its own venv"
	@echo "  check  - fmt-check + vet + lint + cyclo + ineffassign + misspell + test + test-annotator + build"
	@echo "  ci     - fmt-check + vet + cyclo + ineffassign + misspell + test + test-annotator (CI-safe default)"
	@echo "  build-elevenlabs-bridge - build the ElevenLabs webhook bridge binary"
	@echo "  conformance      - run black-box conformance tests (tests/conformance/)"
	@echo "  benchmark        - run the large-corpus retrieval benchmark"
	@echo "  inspector-smoke  - build and run MCP inspector headless smoke test"

# Go trees the two formatting targets cover. `fmt` rewrites them, `fmt-check`
# only reads them.
GOFMT_ROOTS := cmd internal tests

# Developer convenience: format in place. Never a prerequisite of a gate.
fmt:
	gofmt -w $$(find $(GOFMT_ROOTS) -name '*.go')

# The formatting gate, and the reason it is separate from `fmt`. `check` used
# to depend on `fmt`, so unformatted code was rewritten instead of reported: on
# CI the rewrite landed on a throwaway checkout, the job went green, and the
# committed tree stayed unformatted until main needed a cleanup commit (#649).
# A check that mutates the tree can never fail, so this one only reads.
fmt-check:
	@drift=$$(gofmt -l $$(find $(GOFMT_ROOTS) -name '*.go')) || exit 1; \
	if [ -n "$$drift" ]; then \
		echo "gofmt: these files are not formatted:"; \
		printf '  %s\n' $$drift; \
		echo "run 'make fmt' and commit the result"; \
		exit 1; \
	fi

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is required. Install: https://golangci-lint.run/welcome/install/" && exit 1)
	golangci-lint run

cyclo:
	@test -x "$(GOCYCLO_BIN)" || (echo "gocyclo is required. Install: go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0" && exit 1)
	"$(GOCYCLO_BIN)" -over 15 cmd internal tests

ineffassign:
	@test -x "$(INEFFASSIGN_BIN)" || (echo "ineffassign is required. Install: go install github.com/gordonklaus/ineffassign@v0.2.0" && exit 1)
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
# tests/mcp joins the list for #696: session lifetime transitions are ordered
# across goroutines now, and the regression test for that ordering is only a
# durable guard if it keeps running under the race detector.
test-race:
	CGO_ENABLED=1 go test -race ./internal/cli/... ./internal/index/... ./tests/cli ./tests/index ./tests/mcp ./tests/store

test-release-tools:
	cd scripts && python3 -m unittest discover -p 'test_*.py'

conformance:
	go test ./tests/conformance/...

# The reference recognition backend (design 0004) is Python and its suite is
# not reachable from `go test`, so it needs its own target. Core is stdlib-only
# by design; the `ocr` extra is installed as well so the vision recognizers'
# tests run rather than skip (#787).
#
# The install goes into a venv this target owns, not into the ambient
# interpreter. `pip install` into a PEP 668 interpreter (Homebrew, Debian, most
# distribution pythons) is refused outright, so the previous form could not run
# on a normal developer machine: the target existed and was unrunnable, which is
# half of why the suite stayed outside every gate (#649). The venv also keeps
# the editable install off the developer's own environment.
ANNOTATOR_VENV ?= $(CURDIR)/annotator/.venv-test
# Absolute, because the pytest recipe runs from `annotator/`: an override given
# as a relative path would otherwise be resolved against the wrong directory.
ANNOTATOR_PY := $(abspath $(ANNOTATOR_VENV))/bin/python
# Stamp file, so the install runs when the dependency set changes and not on
# every test run. Delete the venv (or run `make clean-all`) to rebuild it from
# scratch, which is also the fix if its interpreter is ever removed underneath.
ANNOTATOR_STAMP := $(ANNOTATOR_VENV)/.installed

$(ANNOTATOR_STAMP): annotator/pyproject.toml
	python3 -m venv "$(ANNOTATOR_VENV)"
	"$(ANNOTATOR_PY)" -m pip install --quiet -e '$(CURDIR)/annotator[test,ocr]'
	touch "$@"

test-annotator: $(ANNOTATOR_STAMP)
	cd annotator && "$(ANNOTATOR_PY)" -m pytest -q

# `check` is the documented local merge-readiness gate, so it has to cover the
# whole repository: the annotator suite is part of it, and the formatting step
# reports rather than rewrites.
check: fmt-check vet lint cyclo ineffassign misspell test test-release-tools test-annotator build

ci: fmt-check vet cyclo ineffassign misspell test test-release-tools test-annotator

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
	rm -rf "$(ANNOTATOR_VENV)"
