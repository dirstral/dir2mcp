# AGENTS.md

## Purpose

Operational guide for coding agents working in this repository.

## Before you start

- Re-check whether the requested plan still matches the current codebase before making changes.
- Review relevant context first: `README.md`, `docs/SPEC.md`, and the affected `cmd/`, `internal/`, and `tests/` paths.
- Preserve existing architecture and conventions unless the issue explicitly requires a refactor.

## Project summary

dir2mcp is a single-binary Go service that indexes local directory content and serves it over MCP with retrieval tools, provenance/citations, and optional x402 request gating.

## Repo map

- `cmd/dir2mcp` - CLI binary entrypoint
- `internal/cli` - command handlers and runtime wiring
- `internal/config` - config precedence and validation
- `internal/ingest` - ingestion + derived representations
- `internal/retrieval` - retrieval/search/ask/open_file services
- `internal/mcp` - MCP server and tool handlers
- `internal/mistral` - Mistral provider client code
- `internal/x402` - payment types and facilitator HTTP client
- `internal/store` - sqlite metadata storage
- `tests` - integration/system tests by area
- `docs/` - reference documentation (SPEC, VISION, ECOSYSTEM, x402 adapter spec)

## Git workflow

- Pull latest `main` before starting implementation work.
- Create an issue branch if one does not already exist (e.g. `issue-<number>-<short-slug>`).
- Keep commits scoped and atomic; use separate commit messages per logical change.
- Do not mention yourself in commit messages.
- Include the issue number in the pull request title.
- Do not push directly to `main`.

## Mandatory commands

Run before committing:

```bash
coderabbit review
```

Run before proposing merge:

```bash
make check
```

Useful focused checks:

```bash
go test ./tests/mcp -run X402
go test ./tests/x402
go test ./tests/cli
```

### Integration tests

Skipped by default. To run:

```bash
RUN_INTEGRATION_TESTS=1 \
MISTRAL_API_KEY=... \
    go test -v ./internal/mistral -run Integration
RUN_INTEGRATION_TESTS=1 \
MISTRAL_API_KEY=... \
MISTRAL_OCR_SAMPLE=/path/to/file.pdf \
    go test -v ./tests -run MistralOCR
RUN_INTEGRATION_TESTS=1 \
MISTRAL_API_KEY=... \
MISTRAL_STT_SAMPLE=/path/to/file.mp3 \
    go test -v ./tests -run MistralSTT
```

## MCP dev servers (Codex)

```bash
codex mcp add everything -- npx -y @modelcontextprotocol/server-everything
codex mcp add sequential-thinking -- npx -y @modelcontextprotocol/server-sequential-thinking
codex mcp add playwright -- npx -y @playwright/mcp
codex mcp add github --url https://api.githubcopilot.com/mcp/ --bearer-token-env-var GITHUB_PERSONAL_ACCESS_TOKEN
codex mcp add context7 -- npx -y @upstash/context7-mcp
```

## Coding rules

- Work only in this repository unless explicitly instructed otherwise.
- Keep patches minimal and issue-focused.
- Do not silently change API/schema/error contracts.
- Maintain clear, machine-parseable errors for MCP/x402 paths.
- Never hardcode API keys, auth tokens, or provider base URLs; keep all credentials env-backed.
- Never introduce secret leakage in logs or error payloads.
- Do not add extra markdown files unless explicitly requested for the task.
- Keep dependency additions minimal and justified.
- Update tests and docs together when behavior changes.

## Tests

- Keep all test files in the `tests/` folder.
- Do not add new `*_test.go` files under `cmd/` or `internal/`.
- Ensure new test coverage follows existing patterns in `tests/`.

## Review/merge readiness

- All relevant tests pass (`make check` green).
- `README.md` and `docs/` are aligned with real behavior.
- No unrelated refactors in issue PRs.
- Implementation aligns with `docs/SPEC.md` and current code behavior, not stale assumptions.
- Secure defaults and existing auth/public-mode safety behavior are preserved.

## Important behavior notes

- Usage/help: no `help` subcommand; invoke `dir2mcp` for usage text.
- Security: local bind default; public mode enforces auth by default.
- x402 mode:
  - `off` = disabled
  - `on` = fail-open on incomplete payment config
  - `required` = strict gating/validation
- `internal/retrieval/engine.go` `Ask()` is a stub — tracked in #70. Use `retrieval.Service` for retrieval work.
- `retrieval.Service.Stats()` returns `ErrNotImplemented` — tracked in #71.
