<p align="center">
  <img src="assets/logo.png" alt="dir2mcp logo" width="720" />
</p>

<p align="center">
  <a href="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml"><img src="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml/badge.svg" alt="CI"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.25+-00ADD8?logo=go" alt="Go 1.25+"></a>
  <a href="https://goreportcard.com/report/github.com/Dirstral/dir2mcp"><img src="https://goreportcard.com/badge/github.com/Dirstral/dir2mcp" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
</p>

# dir2mcp

Deploy any local directory as an MCP knowledge server with indexing, retrieval, and citations, built to maximize use of Mistral models across embedding, OCR, transcription, and generation. Optional layers include ElevenLabs voice output via a dedicated bridge binary and x402 request gating (payment/request-gating protocol).

## Why dir2mcp

- Mistral-first pipeline:
  - Embeddings: `mistral-embed` (text) + `codestral-embed` (code)
  - OCR: `mistral-ocr-latest`
  - STT default: `voxtral-mini-latest`
  - RAG generation: Mistral chat models
- Single Go binary (`dir2mcp`) with local-first state in `.dir2mcp/`
- Companion bridge binary (`elevenlabs-bridge`) for ElevenLabs webhook tools
- MCP Streamable HTTP server with a stable tool surface
- Multimodal ingestion: text/code, document extraction (docling or OCR), transcripts, structured annotations
- Citation-aware retrieval and RAG-style answering
- Optional facilitator-backed x402 payment gating for `tools/call`
- Repo layout with two binaries:
  - `dir2mcp`: MCP server and indexing/runtime host
  - `elevenlabs-bridge`: HTTP helper for ElevenLabs webhook tools
- `dirstral` terminal client is maintained in the separate `dirstral-cli` repo.

## Installation

Install `dir2mcp` via Homebrew tap:

```bash
brew install dirstral/tap/dir2mcp
```

Then verify:

```bash
dir2mcp version
```

Build-from-source remains available as an alternative:

```bash
git clone --recurse-submodules https://github.com/Dirstral/dir2mcp
cd dir2mcp
make build
```

> **Existing clones:** run `git submodule update --init --recursive` to fetch the `dirstral-spec` submodule.
> To update the spec to the latest pinned version: `git submodule update --remote dirstral-spec`.

## Runtime Prerequisites (By Scenario)

Pick the row that matches how you run `dir2mcp`:

| Scenario | Required |
|---|---|
| Local MCP only (`127.0.0.1`) | `dir2mcp` binary, plus either `docling` or `MISTRAL_API_KEY` depending on extractor/generation mode |
| Public MCP (no tunnel) | Local MCP requirements + reachable host/port + secure auth token mode |
| Public MCP via Cloudflare Tunnel | Local MCP requirements + `cloudflared` installed |
| Public MCP via ngrok | Local MCP requirements + `ngrok` installed + verified ngrok account + authtoken |
| x402-gated MCP | Public MCP requirements + facilitator URL + facilitator token + full x402 route policy fields |

## Quickstart

**Build prerequisites (source build only):** Go 1.25+ ([go.dev/dl](https://go.dev/dl/)) and `make`.

```bash
git clone https://github.com/Dirstral/dir2mcp
cd dir2mcp
cp .env.example .env        # add your API keys
# optional: create `.env.local` for local overrides
# (it takes precedence over `.env`)
# cp .env.example .env.local
make build
./dir2mcp up
```

Or build each binary directly:

- `go build -o dir2mcp ./cmd/dir2mcp/`
- `go build -o elevenlabs-bridge ./cmd/elevenlabs-bridge/`

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

Notes:
- `DIR2MCP_DEMO_TOKEN` is required whenever auth is enabled.
- The script now runs the full MCP init sequence (`initialize` -> `notifications/initialized` -> `tools/list` -> `tools/call`).
- If your endpoint is x402-gated, `tools/call` returning HTTP `402` with `PAYMENT-REQUIRED` is treated as healthy.

What it verifies:
- `initialize` returns HTTP 200 and a valid `MCP-Session-Id`
- `tools/list` returns HTTP 200 with tool metadata
- `tools/call` for `dir2mcp_list_files` returns HTTP 200, or HTTP 402 with `PAYMENT-REQUIRED` when x402 is enabled

### Tunnel setup (copy/paste)

Cloudflare quick tunnel (no account-required quick mode):

```bash
cloudflared tunnel --url http://127.0.0.1:8087 --no-autoupdate
```

ngrok (requires verified account + authtoken):

```bash
ngrok config add-authtoken <YOUR_NGROK_TOKEN>
ngrok http http://127.0.0.1:8087
```

Get ngrok public URL from local API:

```bash
curl -sS http://127.0.0.1:4040/api/tunnels \
  | jq -r '.tunnels[] | select(.proto=="https") | .public_url'
```

If you do not have `jq`, copy the public URL from the ngrok web UI (`http://127.0.0.1:4040`).

Then run the hosted smoke probe against either tunnel URL:

```bash
DIR2MCP_DEMO_URL="https://<public-url>/mcp" \
DIR2MCP_DEMO_TOKEN="$(cat .dir2mcp/secret.token)" \
./scripts/smoke_hosted_demo.sh
```

## CLI Commands

| Command | Description |
|---|---|
| `up` | Start the MCP server and begin indexing |
| `status` | Show corpus and indexing state |
| `ask "<question>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `search "<query>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `open-file <rel-path>` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `list-files` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `reindex` | Force full re-ingestion |
| `config init` | Create a baseline `.dir2mcp.yaml` |
| `config print` | Print effective config |
| `install <client>` | Install dir2mcp into a supported MCP client (e.g. `dir2mcp install claude`) |
| `uninstall <client>` | Remove dir2mcp from a supported MCP client |
| `doctor <client>` | Run client-integration diagnostics |
| `print-config <client>` | Print the MCP-server JSON snippet a client expects |
| `version` | Print version |

Running `dir2mcp` with no arguments prints usage, which you can consult anytime to see available commands.
`ask`, `search`, `open-file`, and `list-files` are legacy compatibility shims; new client/orchestrator UX belongs in `dirstral-cli`.

## MCP Tools

| Tool | Description |
|---|---|
| `dir2mcp_search` | Semantic search over indexed content |
| `dir2mcp_ask` | RAG-style question answering with citations |
| `dir2mcp_ask_audio` | Ask with TTS audio response |
| `dir2mcp_transcribe` | Transcribe an audio file from the corpus |
| `dir2mcp_annotate` | Structured annotation of a document |
| `dir2mcp_transcribe_and_ask` | Transcribe then ask over the result |
| `dir2mcp_open_file` | Retrieve a file by path with span context |
| `dir2mcp_list_files` | List indexed files with metadata |
| `dir2mcp_stats` | Corpus statistics |

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
| `MISTRAL_API_KEY` | Conditional | Required for embeddings, Mistral-based extraction/STT, and generation; not required for docling-only read-only extraction flows |
| `DIR2MCP_INGEST_EXTRACTOR` | No | Extraction provider mode: `auto` (default), `docling`, `mistral`, or `off` |
| `DIR2MCP_DOCLING_COMMAND` | No | Optional local command template for document extraction (default: `docling --to md --output - {input}`); when set/available, it is preferred for PDF/image/office-style document extraction |
| `MISTRAL_BASE_URL` | No | Mistral base URL (default: `https://api.mistral.ai`) |
| `DIR2MCP_MISTRAL_MAX_OCR_PAYLOAD_BYTES` | No | Max encoded Mistral upload payload size in bytes for OCR and transcription requests (default: `20971520`); increase for large PDFs or audio files |
| `DIR2MCP_AUTH_TOKEN` | No | Auth token override |
| `DIR2MCP_SERVER_NAME` | No | Override the MCP server name (and suggested `claude mcp add` alias). Defaults to a unique `dir2mcp-<slug>-<6-hex>` derived from the indexed directory |
| `DIR2MCP_SESSION_INACTIVITY_TIMEOUT` | No | Session inactivity timeout (default: `24h`) |
| `DIR2MCP_SESSION_TIMEOUT` | No | Deprecated alias for `DIR2MCP_SESSION_INACTIVITY_TIMEOUT`; still supported but deprecated |
| `DIR2MCP_SESSION_MAX_LIFETIME` | No | Maximum session lifetime |
| `DIR2MCP_HEALTH_CHECK_INTERVAL` | No | Connector health poll interval (default: `5s`) |
| `DIR2MCP_ALLOWED_ORIGINS` | No | Comma-separated additional browser origins |
| `DIR2MCP_X402_FACILITATOR_TOKEN` | No | x402 facilitator bearer token |
| `COHERE_API_KEY` | Optional | When set, auto-enables the post-fusion rerank stage (provider `cohere`); secret, never persisted to the config snapshot |
| `DIR2MCP_RERANK_ENABLED` | No | Tri-state override of the auto behavior: `false` forces reranking off even with a credential; `true` requires it (warns + falls back if no credential) |
| `DIR2MCP_RERANK_MODEL` | No | Cohere rerank model override (default: `rerank-v3.5`) |
| `ELEVENLABS_API_KEY` | No | ElevenLabs key for TTS/STT |
| `ELEVENLABS_BASE_URL` | No | ElevenLabs base URL (default: `https://api.elevenlabs.io`) |

For Homebrew and other installed workflows, you can persist this in `.dir2mcp.yaml`:

```yaml
mistral_max_ocr_payload_bytes: 26214400
ingest_extractor: auto
docling_command: docling --to md --output - {input}
```

Or override for a single run:

```bash
dir2mcp up --mistral-max-ocr-payload-bytes 26214400
```

### Server identity

Each `dir2mcp up` instance reports a unique MCP server name derived from the indexed directory, so running multiple corpora side-by-side (e.g., `dir2mcp-stas-legal-a1b2c3` and `dir2mcp-research-notes-9f44ee`) keeps them distinguishable in your MCP client list.

- Default shape: `dir2mcp-<slug>-<6-hex>`, where `<slug>` is the slugified basename of the indexed directory and `<6-hex>` hashes its absolute path (so the name is stable across re-runs from the same location).
- Developer builds (binaries built locally without a release tag — `dir2mcp version` reports `v0.0.0-dev` or `vdev-<sha>`, optionally with `+dirty`) use a `dir2mcp-dev-<slug>-<6-hex>` prefix instead, so a dev build run from your repo shows up as a distinct entry alongside the brew-installed release in `claude mcp list`.
- Override via YAML (`server.name: my-alias`) or env (`DIR2MCP_SERVER_NAME=my-alias`). Overrides apply verbatim and bypass the default generated name, including the `dir2mcp-dev-...` prefix used by dev builds.
- The `dir2mcp up` banner prints a ready-to-paste `claude mcp add --transport http <name> <url> ...` line using this name; the client-side alias you register with is yours to choose.

### Reranking (optional)

An optional post-fusion **reranking** stage can re-score retrieval candidates with a cross-encoder for higher answer quality. It is **capability-driven** — it activates automatically when a rerank provider credential is present (the same way the Mistral key gates embedding/OCR). A missing credential is a startup condition, not a query failure: in auto mode reranking simply stays off (silent); if you explicitly set `rerank.enabled: true` without a credential the server warns at startup and runs without reranking. Once active it is **fail-open** — any *runtime* provider error (network, rate limit, non-2xx) silently falls back to the normal fused order, so a query never fails because reranking failed. Spec: `dirstral-spec/docs/SPEC.md` §9.1.1.

- Provider: **Cohere** (`POST /v2/rerank`, default model `rerank-v3.5`).
- **Auto-enable**: just provide the credential — `COHERE_API_KEY=...` (or `rerank.cohere.api_key` in YAML). No enable flag required.
- Optional YAML (every field optional; shown with defaults):

  ```yaml
  rerank:
    # `enabled` is an optional override:
    #   omitted -> auto (on iff a credential is present)
    #   false   -> force off even when a credential is present
    #   true    -> require it (warns + falls back if no credential)
    provider: cohere
    candidate_pool: 50      # fused candidates re-scored before truncation to k
    cohere:
      api_key: ${COHERE_API_KEY}
      model: rerank-v3.5
  ```

- Env overrides: `COHERE_API_KEY=...` auto-enables; `DIR2MCP_RERANK_ENABLED=false` opts out even with a credential; `DIR2MCP_RERANK_MODEL=...` overrides the model. Env wins over YAML for the key; the key is a secret and is never written to the config snapshot.
- For `index=both`, reranking is applied once to the merged candidate pool. Ordering is deterministic (relevance desc, then `chunk_id`).

### Auth token behavior

`dir2mcp` bearer auth can come from:

1. `--auth file:<path>` (explicit file source)
2. `DIR2MCP_AUTH_TOKEN` (environment)
3. auto-generated `secret.token` in the state directory (`auth=auto` default)

Operational guidance:
- Do not pass bearer tokens directly on command lines in shared environments.
- Prefer token files (`--auth file:<path>`) or environment variables.

### ElevenLabs bridge

`elevenlabs-bridge` is a separate Go helper that forwards ElevenLabs webhook
calls to a running `dir2mcp` MCP endpoint. It reads its configuration from the
current environment and falls back to sensible defaults:

| Variable | Required | Description |
|---|---|---|
| `MCP_URL` | No | `dir2mcp` MCP endpoint URL. When unset, the bridge first tries `$STATE_DIR/connection.json`, then falls back to `http://127.0.0.1:8087/mcp` |
| `MCP_TOKEN` | No | Explicit bearer token for `dir2mcp`. When set, overrides all other token sources. When unset, the bridge resolves credentials in order: first checks `connection.json.token_file` (inside `$STATE_DIR/connection.json` written by `dir2mcp` when using `--auth file:<path>`), then `$STATE_DIR/secret.token`, and finally falls back to no-auth if none exist |
| `STATE_DIR` | No | `dir2mcp` state directory used to locate `connection.json` and token files. Default: `.dir2mcp` in the bridge working directory |
| `PORT` | No | Bridge listen port. Default: `8088` |

Usage:

```bash
dir2mcp bridge elevenlabs

# Override defaults when needed.
MCP_URL="http://127.0.0.1:8087/mcp" \
STATE_DIR="/path/to/corpus/.dir2mcp" \
PORT=8088 \
dir2mcp bridge elevenlabs

# Legacy wrapper binary still works and forwards to the same integrated command.
make build-elevenlabs-bridge
./elevenlabs-bridge --state-dir /path/to/corpus/.dir2mcp
```

If your `dir2mcp` server already uses `--auth none`, you can omit `MCP_TOKEN`
and `STATE_DIR`.

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

Required fields in `required` mode:
- `--x402-facilitator-url`
- `--x402-resource-base-url`
- `--x402-network` (CAIP-2, for example `eip155:8453`)
- `--x402-price`
- `--x402-scheme`
- `--x402-asset`
- `--x402-pay-to`
- `DIR2MCP_X402_FACILITATOR_TOKEN` (or equivalent secret source)

Minimal example:

```bash
DIR2MCP_X402_FACILITATOR_TOKEN="<token>" \
dir2mcp up \
  --public \
  --listen 0.0.0.0:8092 \
  --x402 required \
  --x402-facilitator-url https://<facilitator> \
  --x402-resource-base-url https://<your-public-host> \
  --x402-network eip155:8453 \
  --x402-price 1000 \
  --x402-scheme exact \
  --x402-asset usdc \
  --x402-pay-to 0x1111111111111111111111111111111111111111
```

If unpaid calls are blocked correctly, `tools/call` returns HTTP `402` plus `PAYMENT-REQUIRED`.

See [dirstral-spec/docs/x402-payment-adapter-spec.md](dirstral-spec/docs/x402-payment-adapter-spec.md) for the full facilitator adapter contract.

## Project Status

Core server, ingestion pipeline, retrieval, citations, and x402 gating are implemented. See [open issues](https://github.com/Dirstral/dir2mcp/issues) for in-progress work.

## Ecosystem Split Status

Issue [#113](https://github.com/Dirstral/dir2mcp/issues/113) tracks the repo split.

- `dir2mcp` (this repo): MCP server implementation + bridge binary
- `dirstral-spec`: canonical specs/schemas/versioning
- `dirstral-conformance`: black-box conformance harness
- `dirstral-cli`: client/orchestrator UX
- `landfall`: code-navigation MCP server product stub

Pre-split audit summary:
- Cross-product imports: none in `dir2mcp` implementation (composition boundary is MCP).
- CLI boundary: `internal/cli` here is server/bootstrap CLI; client UX belongs to `dirstral-cli`.
- Landfall source: product scope comes from issue [#112](https://github.com/Dirstral/dir2mcp/issues/112), now represented by a separate stub repo.
- Docs destination mapping:
  - `docs/SPEC.md` -> `dirstral-spec/docs/SPEC.md`
  - `docs/ECOSYSTEM.md` -> `dirstral-spec/docs/ECOSYSTEM.md`
  - `docs/x402-payment-adapter-spec.md` -> `dirstral-spec/docs/x402-payment-adapter-spec.md`

CLI ownership/disposition matrix:

| Path group | Ownership | Disposition |
|---|---|---|
| `internal/cli/up.go`, `internal/cli/reindex.go`, `internal/cli/status.go` | `dir2mcp` | keep |
| `internal/cli/config_cmd.go`, `internal/cli/bridge.go` | `dir2mcp` | keep |
| `internal/cli/ask.go`, `internal/cli/remote_commands.go` | `dirstral-cli` UX concern | keep as protocol-facing compatibility shims (legacy) |
| `tests/cli/*` for server/bootstrap commands | `dir2mcp` | keep |
| `tests/cli/*` for legacy remote/client-style commands | transition coverage | keep until full extraction is complete |

## Documentation

Normative docs are maintained in the [`dirstral-spec`](https://github.com/dirstral/dirstral-spec) submodule (single source of truth; the `docs/*.md` files here are pointer stubs):

- [dirstral-spec/docs/VISION.md](dirstral-spec/docs/VISION.md) — product vision and strategic direction
- [dirstral-spec/docs/SPEC.md](dirstral-spec/docs/SPEC.md) — normative behavior, schemas, and runtime contracts
- [dirstral-spec/docs/ECOSYSTEM.md](dirstral-spec/docs/ECOSYSTEM.md) — ecosystem/market/discovery/payment context
- [dirstral-spec/docs/x402-payment-adapter-spec.md](dirstral-spec/docs/x402-payment-adapter-spec.md) — facilitator adapter contract
- dir2mcp implements spec version `0.5.x` ([versioning](dirstral-spec/spec/versioning.md))

## Development

```bash
make check        # fmt + vet + lint + cyclo + test + build
make cyclo        # gocyclo -over 15 ./internal/ (install: go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0)
make build        # build dir2mcp binary
make build-elevenlabs-bridge  # build ElevenLabs bridge wrapper binary
make benchmark    # run retrieval benchmarks
```

Release automation:
- Pushing a `v*` tag triggers `.github/workflows/release.yml` and publishes artifacts via GoReleaser.
- Homebrew formula updates require `HOMEBREW_TAP_GITHUB_TOKEN` with write access to `dirstral/homebrew-tap`.

API notes:
- `retrieval.NewEngine` now requires a context as its first parameter:
  `retrieval.NewEngine(ctx, stateDir, rootDir, cfg)`.
- `Engine.Ask` gained a context-aware variant `AskWithContext`; the
  original `Ask` continues to exist as a thin wrapper for compatibility.

`make check` includes `make lint`, which requires [`golangci-lint`](https://golangci-lint.run/welcome/install/) installed locally.
`make cyclo` runs the cyclomatic-complexity gate used by CI. Go Report Card updates externally after the CI run; if the badge lags, refresh it from the goreportcard.com report page for this repository.

Contributor and agent guides: [AGENTS.md](AGENTS.md) · [CLAUDE.md](CLAUDE.md)

## License

MIT. See [LICENSE](LICENSE).
