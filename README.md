<p align="center">
  <img src="assets/logo.png" alt="dir2mcp logo" width="720" />
</p>

<p align="center">
  <a href="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml"><img src="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml/badge.svg" alt="CI"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.22+-00ADD8?logo=go" alt="Go 1.22+"></a>
  <a href="https://goreportcard.com/report/github.com/Dirstral/dir2mcp"><img src="https://goreportcard.com/badge/github.com/Dirstral/dir2mcp" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
</p>

# dir2mcp

Deploy any local directory as an MCP knowledge server with indexing, retrieval, citations, and optional x402 request gating (x402 is a payment/request‑gating protocol).

## Why dir2mcp

- Single Go binary (`dir2mcp`) with local-first state in `.dir2mcp/`
- MCP Streamable HTTP server with a stable tool surface
- Multimodal ingestion: text/code, OCR, transcripts, structured annotations
- Citation-aware retrieval and RAG-style answering
- Optional facilitator-backed x402 payment gating for `tools/call`
- Monorepo layout with two binaries:
  - `dir2mcp`: MCP server and indexing/runtime host
  - `dirstral`: terminal client (Chat/Voice/Start/Stop MCP Server/Settings)

## Quickstart

**Prerequisites:** Go 1.22+ ([go.dev/dl](https://go.dev/dl/)) and `make`.

```bash
git clone https://github.com/Dirstral/dir2mcp
cd dir2mcp
cp .env.example .env        # add your API keys
# optional: create `.env.local` for local overrides
# (it takes precedence over `.env`)
# cp .env.example .env.local
make build
./dir2mcp up
./dirstral chat
```

`DIRSTRAL_MCP_URL` controls where Chat/Voice connect (local or remote).
Server process management (`server start|status|stop`) is local-only. Use
`dirstral server remote` to probe a remote MCP endpoint without process control.

Or build each binary directly:

- `go build -o dir2mcp ./cmd/dir2mcp/`
- `go build -o dirstral ./cmd/dirstral/`

The server prints its MCP endpoint URL on startup. Point your MCP client at that URL.
Precedence (highest to lowest): shell environment variables > `.env.local` > `.env`.

### Local development environment

`dir2mcp` automatically loads both `.env` and `.env.local` from the working directory; `.env.local` overrides `.env`, and real shell environment variables take ultimate precedence.

### Hosted demo smoke runbook

For a quick hosted readiness check (issue #19 scope), run:

```bash
DIR2MCP_DEMO_URL="https://your-host.example/mcp" \
DIR2MCP_DEMO_TOKEN="<optional-bearer-token>" \
./scripts/smoke_hosted_demo.sh
```

What it verifies:
- `initialize` returns HTTP 200 and a valid `MCP-Session-Id`
- `tools/list` returns HTTP 200 with tool metadata
- `tools/call` for `dir2mcp.list_files` returns HTTP 200, or HTTP 402 with `PAYMENT-REQUIRED` when x402 is enabled

## CLI Commands

| Command | Description |
|---|---|
| `up` | Start the MCP server and begin indexing |
| `status` | Show corpus and indexing state |
| `ask "<question>"` | Run a local RAG query |
| `reindex` | Force full re-ingestion |
| `config init` | Create a baseline `.dir2mcp.yaml` |
| `config print` | Print effective config |
| `version` | Print version |

Running `dir2mcp` with no arguments prints usage, which you can consult anytime to see available commands.

## MCP Tools

| Tool | Description |
|---|---|
| `dir2mcp.search` | Semantic search over indexed content |
| `dir2mcp.ask` | RAG-style question answering with citations |
| `dir2mcp.ask_audio` | Ask with TTS audio response |
| `dir2mcp.transcribe` | Transcribe an audio file from the corpus |
| `dir2mcp.annotate` | Structured annotation of a document |
| `dir2mcp.transcribe_and_ask` | Transcribe then ask over the result |
| `dir2mcp.open_file` | Retrieve a file by path with span context |
| `dir2mcp.list_files` | List indexed files with metadata |
| `dir2mcp.stats` | Corpus statistics |

## Configuration

### YAML configuration (`.dir2mcp.yaml`)

The primary on‑disk configuration file is `.dir2mcp.yaml` (created by `dir2mcp config init`).
Use it for persistent, non‑sensitive settings such as connector definitions, defaults, and other options
you might want to check into source control. Values defined here may be overridden at runtime by
environment variables.

### Environment variables (overrides / secrets)

Sensitive keys and temporary runtime overrides are supplied via environment variables. They take
precedence over entries in the YAML file and are convenient for API keys, tokens, or settings that
vary by deployment. The commonly used variables are:

| Variable | Required | Description |
|---|---|---|
| `MISTRAL_API_KEY` | Yes | Mistral API key for embeddings, OCR, and generation |
| `MISTRAL_BASE_URL` | No | Mistral base URL (default: `https://api.mistral.ai`) |
| `DIR2MCP_AUTH_TOKEN` | No | Auth token override |
| `DIR2MCP_SESSION_INACTIVITY_TIMEOUT` | No | Session inactivity timeout (default: `24h`) |
| `DIR2MCP_SESSION_TIMEOUT` | No | Deprecated alias for `DIR2MCP_SESSION_INACTIVITY_TIMEOUT`; still supported but deprecated |
| `DIR2MCP_SESSION_MAX_LIFETIME` | No | Maximum session lifetime |
| `DIR2MCP_HEALTH_CHECK_INTERVAL` | No | Connector health poll interval (default: `5s`) |
| `DIR2MCP_ALLOWED_ORIGINS` | No | Comma-separated additional browser origins |
| `DIR2MCP_X402_FACILITATOR_TOKEN` | No | x402 facilitator bearer token |
| `ELEVENLABS_API_KEY` | No | ElevenLabs key for TTS/STT |
| `ELEVENLABS_BASE_URL` | No | ElevenLabs base URL (default: `https://api.elevenlabs.io`) |

## Security Defaults

- Default listen address is local (`127.0.0.1:0`)
- `--public` binds to `0.0.0.0` (unless explicit `--listen` is provided)
- `--public` with `--auth none` is rejected unless `--force-insecure` is set
- Browser origins are allowlisted (localhost defaults + explicit additions)

## Optional x402 Mode

x402 is optional and additive. Configure with `--x402 off|on|required` and facilitator settings.

| Mode | Behavior |
|---|---|
| `off` | Disabled (default) |
| `on` | Enabled; fail-open if config is incomplete |
| `required` | Strict validation and gating |

See [docs/x402-payment-adapter-spec.md](docs/x402-payment-adapter-spec.md) for the full facilitator adapter contract.

## Project Status

Core server, ingestion pipeline, retrieval, citations, and x402 gating are implemented. See [open issues](https://github.com/Dirstral/dir2mcp/issues) for in-progress work.

## Documentation

- [docs/SPEC.md](docs/SPEC.md) — output contracts, interfaces, schema and operational details
- [docs/VISION.md](docs/VISION.md) — product direction and architectural principles
- [docs/ECOSYSTEM.md](docs/ECOSYSTEM.md) — discovery, trust, metering, payment ecosystem framing
- [docs/x402-payment-adapter-spec.md](docs/x402-payment-adapter-spec.md) — facilitator-facing x402 adapter contract

## Development

```bash
make check        # fmt + vet + lint + test
make build        # build binary
make benchmark    # run retrieval benchmarks
```

API notes:
- `retrieval.NewEngine` now requires a context as its first parameter:
  `retrieval.NewEngine(ctx, stateDir, rootDir, cfg)`.
- `Engine.Ask` gained a context-aware variant `AskWithContext`; the
  original `Ask` continues to exist as a thin wrapper for compatibility.

`make check` includes `make lint`, which requires [`golangci-lint`](https://golangci-lint.run/welcome/install/) installed locally.

Contributor and agent guides: [AGENTS.md](AGENTS.md) · [CLAUDE.md](CLAUDE.md)

## License

MIT. See [LICENSE](LICENSE).
