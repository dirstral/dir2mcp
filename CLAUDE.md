# CLAUDE.md

## Project

dir2mcp is a Go monorepo for deploying a directory as an MCP knowledge server. It supports indexing, retrieval, citations, and optional x402 request gating (an HTTP 402‑based payment challenge system). See [x402 request gating docs](docs/x402-payment-adapter-spec.md) for details.

## Repository layout

- `cmd/dir2mcp`: binary entrypoint
- `internal/cli`: CLI command orchestration (`up`, `status`, `ask`, `reindex`, `config`, `version`)
- `internal/config`: config load/merge/validation
- `internal/ingest`: file discovery, OCR/transcription/annotation representation generation
- `internal/retrieval`: search/ask/open_file logic
- `internal/mcp`: JSON-RPC/MCP server and tools
- `internal/mistral`: Mistral client adapters
- `internal/x402`: x402 types + facilitator client
- `internal/store`: sqlite-backed metadata persistence
- `tests/*`: integration-style suites by subsystem
- `docs/`: reference documentation (SPEC, VISION, ECOSYSTEM, x402 adapter spec)

## Build and test

- Build: `make build`
- Full checks: `make check`
- CI-safe checks: `make ci`
- Focused suites:
  - `go test ./tests/mcp -run X402`
  - `go test ./tests/x402`
  - `go test ./tests/ingest`

### Integration tests

Integration tests are skipped by default. To run them:

```bash
RUN_INTEGRATION_TESTS=1 MISTRAL_API_KEY=... go test -v ./internal/mistral -run Integration
RUN_INTEGRATION_TESTS=1 MISTRAL_API_KEY=... MISTRAL_OCR_SAMPLE=/path/to/file.pdf go test -v ./tests -run MistralOCR
RUN_INTEGRATION_TESTS=1 MISTRAL_API_KEY=... MISTRAL_STT_SAMPLE=/path/to/file.mp3 go test -v ./tests -run MistralSTT
```

## MCP dev servers (Claude Code)

These are optional local development and testing servers for Claude Code integration. Register them when you want richer tooling (web browsing, sequential reasoning, GitHub access, up-to-date library docs) available to Claude Code during development sessions.

```bash
claude mcp add --transport stdio everything -- npx -y @modelcontextprotocol/server-everything
claude mcp add --transport stdio sequential-thinking -- npx -y @modelcontextprotocol/server-sequential-thinking
claude mcp add --transport stdio playwright -- npx -y @playwright/mcp
claude mcp add --transport stdio github -- npx -y @modelcontextprotocol/server-github
claude mcp add --transport stdio context7 -- npx -y @upstash/context7-mcp
```

## Releasing

Releases are automated via GoReleaser and triggered by pushing a version tag. The tag **must point to a commit on `main`** — tagging mid-PR or branch commits will release whatever that commit contains.

```bash
# 1. Merge all PRs first, then:
git checkout main && git pull
git tag v0.1.0
git push origin v0.1.0
```

The release workflow will build both binaries for `darwin/linux × amd64/arm64`, publish a GitHub release with checksums, and push updated formulas to `Dirstral/homebrew-tap` automatically.

Requires `HOMEBREW_TAP_GITHUB_TOKEN` to be set as a repository secret (a PAT with `repo` scope on `Dirstral/homebrew-tap`).

## Working conventions

- Keep changes scoped to the issue.
- Preserve existing tool/error contracts and structured fields.
- Do not log secrets or raw sensitive payloads.
- If behavior changes, update tests and docs in the same PR.
- Prefer deterministic behavior and explicit error handling.

## PR checklist

- [ ] `make check` passes locally
- [ ] New/changed behavior has test coverage
- [ ] `README.md` and `docs/` remain truthful
- [ ] No unrelated files changed

## Known gotchas

- `dir2mcp` has no `help` subcommand; usage is printed when `dir2mcp` is invoked without arguments or subcommands.
- `--public` requires auth unless `--force-insecure` is explicitly set.
- x402 mode semantics:
  - `off`: disabled
  - `on`: fail-open on incomplete config
  - `required`: strict config validation/gating

