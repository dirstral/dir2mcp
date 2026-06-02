# GEMINI.md

## Project
dir2mcp is a Go monorepo for deploying a directory as an MCP knowledge server. It supports indexing, retrieval, citations, and optional x402 request gating.

## Repository layout
- `cmd/dir2mcp`: binary entrypoint
- `cmd/elevenlabs-bridge`: companion bridge binary
- `internal/cli`: CLI command orchestration (`up`, `down`, `status`, `ask`, `search`, `open-file`, `list-files`, `reindex`, `config`, `doctor`, `install`, `uninstall`, `version`, etc.)
- `internal/config`: config load/merge/validation
- `internal/ingest`: file discovery, OCR/transcription/annotation representation generation
- `internal/retrieval`: search/ask/open_file logic
- `internal/mcp`: JSON-RPC/MCP server and tools
- `internal/mistral`: Mistral client adapters
- `internal/x402`: x402 types + facilitator client
- `internal/store`: sqlite-backed metadata persistence
- `tests/*`: integration-style suites by subsystem
- `dirstral-spec/`: git submodule containing canonical docs (SPEC, VISION, ECOSYSTEM)

## Build and test
- Build: `make build`
- Full checks: `make check`
- CI-safe checks: `make ci`
- Focused suites:
  - `go test ./tests/mcp -run X402`
  - `go test ./tests/x402`
  - `go test ./tests/ingest`
  - `go test ./tests/cli`

### Integration tests
Integration tests are skipped by default. To run them, set `RUN_INTEGRATION_TESTS=1` and provide necessary API keys:
```bash
RUN_INTEGRATION_TESTS=1 MISTRAL_API_KEY=... go test -v ./tests/mistral -run Integration
```

## Working conventions
- **Go Version:** Go 1.25+.
- **Tests:** Add new test files under the `tests/` folder. **DO NOT** add new `*_test.go` files under `cmd/` or `internal/` — some legacy in-package tests still live there, but new coverage goes in `tests/`.
- **Conventions:** Use Conventional Commits for all commit messages.
- **Scope:** Keep changes minimal and focused on the specific issue/task.
- **Contracts:** Preserve existing tool/error contracts and structured fields.
- **Security:** NEVER log secrets, API keys, or raw sensitive payloads. Keep all credentials environment-backed.
- **Documentation:** Update `README.md` and `docs/` (stubs) if behavior changes. Canonical docs live in `dirstral-spec/`.
- **Error Handling:** Prefer deterministic behavior and explicit, machine-parseable error handling.

## PR checklist
- [ ] `make check` passes locally.
- [ ] New/changed behavior has test coverage in `tests/`.
- [ ] `README.md` and `docs/` remain truthful.
- [ ] No unrelated files or refactors changed.
- [ ] Conventional Commits used.

## Known gotchas
- `dir2mcp` has no `help` subcommand; usage is printed when invoked without arguments.
- `--public` requires auth unless `--force-insecure` is explicitly set.
- x402 mode semantics: `off` (disabled), `on` (fail-open on incomplete config), `required` (strict gating).
- Submodules: Ensure `dirstral-spec` is initialized: `git submodule update --init --recursive`.
