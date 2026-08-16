<p align="center">
  <img src="assets/logo.png" alt="dir2mcp logo" width="720" />
</p>

<p align="center">
  <a href="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml"><img src="https://github.com/Dirstral/dir2mcp/actions/workflows/go.yml/badge.svg" alt="CI"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.25+-00ADD8?logo=go" alt="Go 1.25+"></a>
  <a href="https://golangci-lint.run/"><img src="https://img.shields.io/badge/lint-golangci--lint-brightgreen?logo=go&logoColor=white" alt="golangci-lint"></a>
  <a href="https://pkg.go.dev/github.com/dirstral/dir2mcp"><img src="https://pkg.go.dev/badge/github.com/dirstral/dir2mcp.svg" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
</p>

# dir2mcp

Deploy any local directory as an MCP knowledge server with indexing, retrieval, and citations. It works out of the box with one API key, and every capability can be rebound to a different provider without changing how the corpus is indexed or cited. Optional layers include ElevenLabs voice output via a dedicated bridge binary and x402 request gating (payment/request-gating protocol).

## Why dir2mcp

- Provider-agnostic by capability. Embedding, extraction/OCR, transcription,
  generation and reranking are bound independently, so you can mix providers or
  move one capability without touching the rest:

  | Capability | Providers you can bind |
  |---|---|
  | Embedding | Mistral, OpenAI, Gemini, any OpenAI-compatible endpoint, self-hosted |
  | Extraction / OCR | docling (local), docling-serve (HTTP), pandoc, Mistral OCR |
  | Transcription | Voxtral, Whisper (self-hosted or OpenAI-compatible) |
  | Generation | Mistral, OpenAI, Anthropic, Gemini, OpenRouter, any OpenAI-compatible endpoint |
  | Reranking | Cohere, ColBERT, self-hosted |

- A provider activates when its credential is present, so adding a key is the
  only step. There is no separate enable flag to forget. Mistral is the default
  when a Mistral key is the one you supply, which keeps first-run simple without
  making it the only option.
- Runs fully local with no egress: docling for extraction, an OpenAI-compatible
  local server (Ollama, vLLM, llama.cpp, LM Studio, TEI) for embedding and chat.
- Single Go binary (`dir2mcp`) with local-first state in `.dir2mcp/`
- Companion bridge binary (`elevenlabs-bridge`) for ElevenLabs webhook tools
- MCP Streamable HTTP server with a stable tool surface
- Multimodal ingestion: text/code, document extraction (docling or OCR), transcripts, structured annotations
- Video recognition: a pluggable backend turns broadcast footage into time-anchored,
  filterable annotations (`event`, `entities`), each naming the recognizer that
  produced it. An answer cites a moment, and the client can play exactly that moment
- Citation-aware retrieval and RAG-style answering
- Optional facilitator-backed x402 payment gating for `tools/call`
- Repo layout with two binaries:
  - `dir2mcp`: MCP server and indexing/runtime host
  - `elevenlabs-bridge`: HTTP helper for ElevenLabs webhook tools
- `dirstral` terminal client is maintained in the separate `dirstral-cli` repo.

## Installation

Install `dir2mcp` via Homebrew tap:

```bash
brew tap dirstral/tap
brew trust dirstral/tap      # required on Homebrew 6.x: third-party taps are untrusted by default
brew install dirstral/tap/dir2mcp
```

On Homebrew 6.x a freshly tapped third-party formula is refused until the tap is trusted, so `brew install dirstral/tap/dir2mcp` on a clean machine fails without the `brew trust` step above. (On older Homebrew the trust step is a harmless no-op.)

Then verify:

```bash
dir2mcp version
```

### Install tracks: `dir2mcp` vs `dir2mcp-full`

dir2mcp ships in two Homebrew formulas that install the **same binary** but differ in whether the [docling](https://github.com/docling-project/docling) structured-extraction runtime is bundled:

| Track | Install | docling | Footprint | Pick when |
|---|---|---|---|---|
| **Lean** (default) | `brew install dirstral/tap/dir2mcp` | **Not bundled** — bring your own | installs in ~seconds (only `libcap` + `bubblewrap`) | You already have `docling`, run a `docling-serve` container, extract via Mistral OCR, or index docling-free corpora |
| **Full** | `brew install dirstral/tap/dir2mcp-full` | **Bundled** (docling runtime included) | ≈ 6.3 GB installed / ~3 min build | You want local structured PDF/image extraction with zero extra setup |

The two formulas install the **same binary**, so they are mutually exclusive — both provide a `dir2mcp` runtime and Homebrew refuses to have both linked at once. To **switch tracks**, first unlink (or uninstall) the currently-installed one:

```bash
brew unlink dir2mcp        # or: brew uninstall dir2mcp
brew install dirstral/tap/dir2mcp-full
```

**Full footprint:** the full formula bundles a Python 3.12 venv with docling, torch/torchvision, scipy, and shapely, pulling a large dependency chain (llvm, rust, python, openssl, …). Measured at ≈ 6.3 GB installed (~39k files) and ~3 min to build on Linux x86_64 (Homebrew 6.0.6); macOS and prebuilt-bottle installs will differ. The lean formula, by contrast, installs in seconds with only `libcap` + `bubblewrap`.

Choose **full** for batteries-included local extraction; choose **lean** if you bring docling yourself, run docling-serve, or rely on Mistral OCR. Either way, extraction is configurable at runtime via `ingest.extractor` (see [Document extraction](#document-extraction-modes--fallback)). To move from lean to full (or to a shared docling-serve) in stages without a re-index flag day, see [Migration & rollout](#migration--rollout-adopting-docling-in-stages).

### Nix (macOS + Linux)

A [Nix flake](flake.nix) packages the **lean** `dir2mcp` binary for `x86_64`/`aarch64` on both Linux and macOS. Run it without installing:

```bash
nix run github:dirstral/dir2mcp -- version
```

Or add it to a profile:

```bash
nix profile install github:dirstral/dir2mcp
```

The flake builds the lean binary only (no bundled docling runtime); for batteries-included structured extraction, use the `dir2mcp-full` Homebrew formula or the docling-full container. A `devShells.default` with the Go toolchain is also exposed (`nix develop`). The flake also exposes `overlays.default` (adds `pkgs.dir2mcp` to any nixpkgs), plus `darwinModules.default` and `homeManagerModules.default` for the declarative service below.

#### Declarative service (nix-darwin / home-manager)

The flake ships service modules that run `dir2mcp up --foreground` under a supervisor (launchd on macOS, systemd `--user` on Linux), so the corpus server starts at login and is restarted on failure. These require the flake's modules — they are not part of nixpkgs.

Add the input and the module to your config. **nix-darwin:**

```nix
{
  inputs.dir2mcp.url = "github:dirstral/dir2mcp";

  # in your darwinConfiguration modules list:
  modules = [
    dir2mcp.darwinModules.default
    {
      services.dir2mcp = {
        enable = true;
        rootDir = "/Users/me/Documents/corpus";
        # Secrets (MISTRAL_API_KEY, OPENAI_API_KEY, DIR2MCP_AUTH_TOKEN, ...)
        # live in this file, NOT in the nix store. Manage it yourself with
        # restrictive permissions.
        environmentFile = "/Users/me/.config/dir2mcp/env";
      };
    }
  ];
}
```

**home-manager** (works on macOS via launchd and on Linux via systemd `--user`):

```nix
{
  inputs.dir2mcp.url = "github:dirstral/dir2mcp";

  # in your homeConfiguration modules list:
  modules = [
    dir2mcp.homeManagerModules.default
    {
      services.dir2mcp = {
        enable = true;
        rootDir = "/home/me/corpus";
        environmentFile = "/home/me/.config/dir2mcp/env";
      };
    }
  ];
}
```

Optional knobs: `stateDir`, `listen`, `extraArgs` (e.g. `[ "--public" "--auth" "auto" ]`), and `package` (defaults to this flake's lean build). On macOS, launchd has no native `EnvironmentFile`, so the module sources `environmentFile` via a small wrapper script at start; on Linux it is wired to systemd's native `EnvironmentFile=`. Either way the secret values stay outside the world-readable nix store.

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

> **Port note:** `dir2mcp up` binds a **random** loopback port by default (`127.0.0.1:0`) and prints its actual MCP URL on startup — it is not `:8087`. Either substitute the printed port in the commands below, or pin a fixed port first with `dir2mcp up --listen 127.0.0.1:8087` (or `listen_addr: 127.0.0.1:8087` in `.dir2mcp.yaml`). The `:8087` below is a placeholder for whichever local port your server is actually listening on.

Cloudflare quick tunnel (no account-required quick mode):

```bash
cloudflared tunnel --url http://127.0.0.1:<PORT> --no-autoupdate \
  --http-host-header 127.0.0.1:<PORT>
```

> **`--http-host-header` is required, not optional.** The MCP SDK auto-enables
> DNS-rebinding protection for a loopback-bound server, and then refuses any
> request whose `Host` header is not a loopback name. A tunnel forwards the
> PUBLIC hostname by default, so without this flag **every request returns 403**
> and the body does not say why. The flag makes the forwarded `Host` truthful and
> keeps the protection ON. Do not turn the protection off instead. Verified on a
> live deployment (issue #853). For the full explanation, for nginx, Caddy and
> Traefik, and for what each alternative costs, read
> [Reverse proxy and tunnel: the `Host` header](#reverse-proxy-and-tunnel-the-host-header).

A quick tunnel gets a NEW random hostname every time it starts, including after a
reboot, so treat the URL as ephemeral and re-read it from the running process
rather than from an earlier log line.

ngrok (requires verified account + authtoken):

```bash
ngrok config add-authtoken <YOUR_NGROK_TOKEN>
ngrok http http://127.0.0.1:<PORT> --host-header=127.0.0.1:<PORT>
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

### Reverse proxy and tunnel: the `Host` header

**Symptom.** The server starts, the token is correct, and every request through the proxy fails, including `initialize`:

```text
403 Forbidden: invalid Host header "example.trycloudflare.com"
```

**Cause.** The MCP Go SDK enables DNS-rebinding protection for a connection that arrives on a loopback address. It then refuses any request whose `Host` header is not a loopback name. The guard is correct and should stay on: a browser page loaded from a public name that reaches a private loopback server is exactly the attack it stops. A reverse proxy has the same shape as that attack, and the guard cannot tell the two apart, because a proxy forwards the public hostname by default.

The default `listen_addr` is `127.0.0.1:0`, a loopback address, so every operator who puts a proxy in front of the default configuration meets this on the first request.

**Recommended fix: make the forwarded `Host` truthful.** Tell the proxy to send the address it really connects to. The protection stays on. Substitute your own port for `8087` in every row.

| Proxy | Setting |
|---|---|
| cloudflared (`--url` mode) | `--http-host-header 127.0.0.1:8087` |
| cloudflared (ingress rules) | `originRequest.httpHostHeader: 127.0.0.1:8087` |
| ngrok | `--host-header=127.0.0.1:8087` (deprecated by ngrok; see the traffic policy below) |
| nginx | `proxy_set_header Host 127.0.0.1:8087;` |
| Caddy | `reverse_proxy 127.0.0.1:8087 { header_up Host {upstream_hostport} }` |
| Traefik | `passHostHeader: false` on the service load balancer |

The cloudflared `--url` row is measured end to end against a quick tunnel. The other rows come from each proxy's own documentation and are not measured here.

The cloudflared flag applies only to `--url` mode. With ingress rules, set the same value under `originRequest` instead:

```yaml
ingress:
  - hostname: mcp.example.com
    service: http://127.0.0.1:8087
    originRequest:
      httpHostHeader: 127.0.0.1:8087
  - service: http_status:404
```

ngrok marks `--host-header` deprecated and points at a traffic policy instead. The `add-headers` action treats `host` as a replacement, not an addition:

```yaml
# ngrok traffic policy
on_http_request:
  - actions:
      - type: "add-headers"
        config:
          headers:
            host: "127.0.0.1:8087"
```

Any loopback name passes the guard: `localhost`, `127.0.0.1` and `[::1]` all work, and the port in the header is not compared. Send the real address anyway, so the value stays honest in logs and after a port change.

**Alternative: bind a non-loopback address.** The guard runs only when the connection arrives on a loopback address. Start the server on a routable address, then point the proxy at that address:

```bash
dir2mcp up --listen 0.0.0.0:8087
# point the proxy at http://<host-lan-ip>:8087, NOT at http://127.0.0.1:8087
```

`--public` sets `0.0.0.0` for you, but it does not fix this on its own. A proxy that still connects to `127.0.0.1` still arrives on a loopback address, and the guard still refuses it. The trade is that the port becomes reachable from the whole network: keep auth on (`--public` requires auth unless `--force-insecure` is set) and restrict the port with a firewall.

**Not recommended: turn the guard off.** The SDK reads `MCPGODEBUG=disablelocalhostprotection=1` and then skips the check for the whole process. dir2mcp has no setting of its own for it. Know the trade before you use it: the server then accepts any `Host` from anywhere, so a page in a browser on the same machine can reach the endpoint through a rebound DNS name. The bearer token is the only thing that stops that page, which makes `--auth none` (or a leaked token) an open door. The SDK also states that it removes this switch in v1.6.0, so a deployment that depends on it breaks at the next SDK update. Fix the proxy instead.

**These settings do not help, in spite of the names.**

- `allowed_origins` lists the browser `Origin` values this server accepts (CORS and CSRF). It never reads the `Host` header.
- `trusted_proxies` lists the CIDRs whose `X-Forwarded-For` the rate limiter may believe. It never reads the `Host` header.

## CLI Commands

| Command | Description |
|---|---|
| `up` | Start the MCP server and begin indexing (daemonizes by default) |
| `down` | Stop the dir2mcp server running in this directory |
| `status` | Show corpus and indexing state |
| `ask "<question>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `search "<query>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `open-file <rel-path>` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `list-files` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `reindex` | Force full re-ingestion. `--embeddings-only` instead retries just the chunks that failed to embed (see [Recovering from a failed embed run](#recovering-from-a-failed-embed-run)) |
| `embed-worker` | Run a standalone distributed embed worker (no MCP serving; requires a Tier-C store + broker) |
| `export` | Render a transcript as VTT/SRT/TTML subtitles (`export --format vtt\|srt\|ttml <path>`) |
| `bridge` | Run helper adapters (for example the ElevenLabs webhook bridge) |
| `support-bundle` | Collect logs + config + status into a shareable `tar.gz` (owner-only; credentials always redacted, local paths/endpoints redacted unless `--include-content` — see [What a support bundle discloses](#what-a-support-bundle-discloses)) |
| `config init` | Interactive setup wizard (on a TTY): prompts for provider API keys, where to store them (`.env.local` or the OS keychain), and a corpus profile, then writes/updates `.dir2mcp.yaml`. Non-interactive (`--non-interactive`/`--json`/`--quiet`/no TTY) just writes a baseline config. `dir2mcp up` also launches this wizard on first run when started interactively (a TTY, and not `--json`/`--non-interactive`/read-only) and no embedding provider resolves. |
| `config print` | Print effective config |
| `config set-secret <ENV_VAR>` | Store a provider credential in the OS keychain (encrypted at rest) instead of a plaintext `.env.local` |
| `config rm-secret <ENV_VAR>` | Remove a credential from the OS keychain |
| `config secrets` | Show which provider credentials are present in the keychain / environment (never prints values) |
| `install <client>` | Install dir2mcp into a supported MCP client (e.g. `dir2mcp install claude`) |
| `uninstall <client>` | Remove dir2mcp from a supported MCP client |
| `doctor [<client>]` | With a client name, run client-integration diagnostics. With no argument, run a server-side preflight (config, provider resolution, an **egress** check reporting whether any resolved provider is a public/third-party host, extractor availability, indexing failures); add `--deep` to actively probe the embedding credential |
| `print-config <client>` | Print the MCP-server JSON snippet a client expects |
| `service install\|uninstall\|status` | Auto-start the daemon at login so the corpus survives a reboot (macOS launchd) |
| `version` | Print version |

Running `dir2mcp` with no arguments prints usage, which you can consult anytime to see available commands.
`ask`, `search`, `open-file`, and `list-files` are legacy compatibility shims; new client/orchestrator UX belongs in `dirstral-cli`.

### Recovering from a failed embed run

When the embedding provider rejects a request for a reason that is not the chunk's fault (a key revoked, rotated or billing-suspended mid-run, a quota that went hard-limit, an upstream outage), the affected chunks are recorded as failed and are **not** retried on their own. Restarting the daemon with a working credential does not help by itself: the chunks are in an error state, not a pending one, so `status` reports `embedded_pending=0, errors=N` and the worker sits idle.

Fix the provider first, then re-queue the failed chunks:

```bash
dir2mcp reindex --embeddings-only                          # retry the provider-side failures
dir2mcp reindex --embeddings-only --error-category auth    # or just one category
```

This re-runs only the embed step: extraction (OCR, transcription, media analysis) is **not** repeated, which is the whole point — extraction is usually the expensive half and its output has not changed. It is safe to run while the daemon is up; the running embed worker picks the chunks up on its next cycle. With no daemon running, start one with `dir2mcp up`.

By default it retries the categories a provider fix can plausibly clear: `auth`, `rate_limit`, `transient_net`, and `unknown` (the catch-all for failures the classifier could not label). Failures that are a property of the stored input — `payload_too_large`, `parse_error`, `embedding_failure`, `quality_gate` — are left alone, because re-sending identical bytes to the same provider just fails again; those need a real re-ingest (`dir2mcp reindex`) after changing the input or the configuration. `dir2mcp doctor` names the retryable count when there is one.

### Auto-start at login (macOS)

`dir2mcp up` runs a background daemon, but it does not come back on its own after a reboot. `dir2mcp service install` registers a per-corpus launchd agent that restarts `dir2mcp up --foreground` at every login (and on crash), so the MCP server stays connected across reboots. Use `service status` to inspect it and `service uninstall` to remove it.

The launchd job starts from a clean environment and will **not** inherit a `MISTRAL_API_KEY` you only `export`ed in a shell. Persist the credential first with `dir2mcp config init` (writes `.env.local` in the corpus directory) so the booted daemon can find it; `service install` warns when no persisted credential is present.

`service install` sweeps **every** credential the effective config needs, not just provider API keys: the S3 source credentials (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`), `QDRANT_API_KEY`, `DIR2MCP_INDEX_PGVECTOR_DSN`, `DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL`, and `DIR2MCP_X402_FACILITATOR_TOKEN`. These have no config-file home by design, so the environment / keychain / `.env.local` is their only source. Any of them found in your current shell is copied into `.env.local`; any that is **required** by the config and has no persistent source is named in a warning (and in `missing_credentials` under `--json`) — the service is installed, but it will not boot until you give that secret a persistent source. Values are never printed.

Install and uninstall are also fail-safe: a supervisor step that fails during install rolls the previous service definition (and its loaded/enabled state) back, and `uninstall` refuses to delete a unit whose daemon it could not verifiably stop, so a running daemon is never orphaned. On Linux, `service status` returns an error rather than reporting `installed, not running` when `systemctl --user` cannot answer (no user bus, permission denied, systemctl missing).

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
| `dir2mcp_open_media_clip` | Extract the audio/video snippet for a media hit (time span) |
| `dir2mcp_related` | Find chunks related to a chunk you already have |
| `dir2mcp_list_files` | List indexed files with metadata |
| `dir2mcp_stats` | Corpus statistics, including the extraction engine actually in use |

### What a citation carries

A `time` span (recognition and media content) carries more than its bounds, and a
client can show all of it:

| Field | Meaning |
|---|---|
| `start_ms` / `end_ms` | the span bounds, so a client can play exactly the cited moment |
| `event` | the structured event the annotation records, for example `home_run`. Filterable |
| `entities` | the entities the annotation names, for example `player:curtis-mead`. Filterable |
| `sources` | which recognizer produced the annotation, for example `["playbyplay"]` or `["scorebug","face"]`. Provenance only, never a ranking signal |
| `derivation` | `observed` or `generated`. A client MUST NOT present a `generated` span as a record of what happened |

`sources` and `derivation` answer different questions. `derivation` says whether a
span RECORDS or DESCRIBES. `sources` says WHICH component produced it, which
matters when two recognizers both observed and disagree. Both are optional and
omitted when absent, never served as `null` or `[]`.

## Configuration

### YAML configuration (`.dir2mcp.yaml`)

The primary on‑disk configuration file is `.dir2mcp.yaml` (created by `dir2mcp config init`).
Use it for persistent, non‑sensitive settings such as connector definitions, defaults, and other options
you might want to check into source control. Values defined here may be overridden at runtime by
environment variables.

A key that no setting claims does not fail the load. `dir2mcp up` prints one warning that
names every unrecognized key, then starts, so a typo or a stale key from an older release
is reported instead of being dropped in silence.

#### Chunking keys

```yaml
chunking:
  max_tokens: 0        # 0 = the chunker default
  overlap_tokens: 0    # must be smaller than max_tokens when max_tokens is set
```

`chunking.strategy` is **not** a setting. Releases before this one accepted the key, saved
it, and never read it: chunking is selected per document type (characters for text, line
windows for code, time windows for a transcript), and the canonical spec defines no
strategy selector. The key is now unrecognized, so a config that still carries it loads
with the warning above and no longer publishes the key back into the saved config or the
effective snapshot.

#### Ingest size cap

```yaml
ingest:
  max_file_mb: 20      # default: 20
```

`ingest.max_file_mb` is the per-file size policy, and it is enforced as a bound on the
reads themselves, not only as a check at discovery. Discovery refuses a file over it, and
so does every read that follows: the document read, the subtitle sidecar, the
object-store download, the multimodal media read, and the on-demand `annotate` /
`transcribe` reads. A check only measures a file at one instant; a file that grows
afterwards, or an object that serves more bytes than it listed, is caught by the read.
(`open_file` answers a window rather than a whole file, so its raw-text read carries its
own budget derived from `max_chars`; the cap still bounds the source read it needs to
locate cached extracted text.)

During indexing a file over the cap is a **skip, not an error**: it keeps a visible
`skipped` row with reason `size_cap`, it is counted in the skip breakdown `dir2mcp status`
reports, and its chunks leave retrieval. It was refused by policy, not by a failure, so it
never lands in the failure list. On a tool request the refusal is reported as
`FILE_TOO_LARGE`, which names the setting instead of a generic failure.

Raw text now follows this setting exactly. Earlier releases gated raw-text indexing on a
hard-coded 10 MiB, so a 15 MiB text file was admitted by the configured cap and then
failed anyway. Text, code, markdown, data and HTML files between 10 MiB and the
configured cap are indexed. To keep the old ceiling, set `max_file_mb: 10`.

Zero or a negative value is not "unlimited": it selects the built-in default bound.

#### Retrieval answer keys

```yaml
rag:
  generate_answer: true   # false serves every ask as search-only
  k_default: 15           # hits for a request that sends no k (1..50)
```

`rag.k_default` sets how many hits a request that omits `k` gets. It applies to every
tool that takes a `k`: `search`, `ask`, `related`, `ask_audio` and `transcribe_and_ask`,
plus the `ask` and `search` CLI commands. Precedence is fixed: the `k` on the request,
then `rag.k_default`, then the shipped default of 15.

The value carries the same `1..50` bound as the request field. A value outside it fails
at startup with `CONFIG_INVALID`, because a default that asks for a `k` the tools forbid
would otherwise only fail later, on a request the operator never wrote.

The served tool schemas advertise the **effective** value. `tools/list` reports
`"default": <your k_default>` for `k`, so a client that reads the schema and sends the
advertised number explicitly gets the same result as a client that omits the field.

`rag.generate_answer: false` turns answer generation off for the whole server. Every
`ask`, `ask_audio` and `transcribe_and_ask` request then behaves as `mode=search_only`:
the response shape is unchanged, `answer` is `""`, `citations` is `[]`, and the retrieval
hits are still returned. No chat provider is called. A request cannot switch generation
back on, so `mode=answer` is served as search-only rather than refused.

### Environment variables (overrides / secrets)

Sensitive keys and temporary runtime overrides are supplied via environment variables. They take
precedence over entries in the YAML file and are convenient for API keys, tokens, or settings that
vary by deployment. The commonly used variables are:

| Variable | Required | Description |
|---|---|---|
| `MISTRAL_API_KEY` | Conditional | Required for embeddings, Mistral-based extraction/STT, and generation; not required for docling-only read-only extraction flows |
| `DIR2MCP_SKIP_EMBED_PROBE` | No | When set to any non-empty value, skips the startup embedding-credential probe (a one-shot embed that validates the key/model before serving). Provider resolution and adapter-build checks still run; only the network probe is skipped, so an invalid credential resurfaces at first real embed instead of at startup. Intended for air-gapped bring-up and hermetic CI |
| `DIR2MCP_INGEST_EXTRACTOR` | No | Extraction mode: `auto` (default), `docling`, `docling-serve`, `mistral`, or `off` |
| `DIR2MCP_DOCLING_COMMAND` | No | Optional local command template for document extraction (default: `docling --to json --output - {input}`); when set/available, it is preferred for PDF/image/office-style document extraction. The default requests structured JSON so ingestion preserves reading order, section hierarchy, and per-element page/bbox provenance (region citations); a custom `--to md` template still works and falls back to flat Markdown |
| `DIR2MCP_DOCLING_SERVE_URL` | No | HTTP endpoint of a running [docling-serve](https://github.com/docling-project/docling-serve) container (e.g. `http://127.0.0.1:5001`). Required when `ingest.extractor=docling-serve`; under `auto` it is used only when the docling CLI is not on `PATH` |
| `DIR2MCP_INGEST_WATCH` | No | When `true`, a running `dir2mcp up` keeps a filesystem watcher live and incrementally indexes added/changed/deleted files (default: `false`) |
| `DIR2MCP_INGEST_WATCH_DEBOUNCE` | No | Per-file debounce window for coalescing editor write bursts before re-indexing (default: `500ms`) |
| _(Mistral endpoint)_ | — | The Mistral base URL is **not** configurable via an environment variable. To proxy Mistral or point at a private/custom endpoint, add a `providers:` entry with a `base_url` (see [Self-hosted / GPU-VPS provider endpoints](#self-hosted--gpu-vps-provider-endpoints-embed--ocr--stt)) |
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
| `GEMINI_API_KEY` | Optional | Google Gemini key; enables the `gemini` provider profile (embeddings, chat, and native STT/TTS via `generateContent`). Bind it explicitly to a capability (e.g. `model.embed.provider: gemini`, `stt.provider: gemini`) — auto-selection still prefers Mistral. Secret, never persisted |

For Homebrew and other installed workflows, you can persist this in `.dir2mcp.yaml`:

```yaml
ingest_extractor: auto
ingest_on_unsupported: lenient   # lenient (default) | strict
docling_command: docling --to json --output - {input}
```

### Storing credentials in the OS keychain

Provider API keys can live in the OS keychain (macOS Keychain, Linux Secret Service,
Windows Credential Manager) — encrypted at rest — instead of a plaintext `.env.local`:

```bash
dir2mcp config set-secret MISTRAL_API_KEY   # hidden prompt, or pipe: op read … | dir2mcp config set-secret MISTRAL_API_KEY
dir2mcp config secrets                       # show which keys are in the keychain / env (never prints values)
dir2mcp config rm-secret MISTRAL_API_KEY
```

Credentials resolve in the order defined by SPEC §16.1.1: **environment variable → OS
keychain → `.env.local` / `.env` file**. So an explicit env var always wins, a keychain
entry beats a plaintext file, and the keychain is consulted only for the built-in provider
keys (`MISTRAL_API_KEY`, `COHERE_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, `OPENROUTER_API_KEY`, `ELEVENLABS_API_KEY`). Keychain access is fail-open
(a missing entry or an unavailable/locked backend simply falls through to the file/env
sources) and can be turned off entirely with `DIR2MCP_DISABLE_KEYCHAIN=1`.

> **Background daemons:** a launchd/systemd service may not be able to unlock the keychain
> unattended. For always-on `service install` deployments, persist the credential to
> `.env.local` first with `dir2mcp config init` (`service install` warns when no persisted
> credential is present); the keychain is most useful for the interactive `dir2mcp up` / CLI.
> Resolution is fail-open, so a daemon that cannot read the keychain falls back to `.env.local`.

### Document extraction: modes & fallback

PDFs and images are converted to text by an **extractor**, selected with `ingest.extractor` (env `DIR2MCP_INGEST_EXTRACTOR`):

| Mode | Behavior |
|---|---|
| `auto` (default) | Prefer the local `docling` CLI; else a reachable `docling-serve`; else Mistral OCR; else disabled. |
| `docling` | Local docling CLI only (fails if not on `PATH`). |
| `docling-serve` | docling-serve HTTP only; requires a reachable `serve_url` (no fallback). |
| `mistral` | Mistral OCR only (requires `MISTRAL_API_KEY`). |
| `off` | No extraction (PDFs/images contribute no extracted text). |

Under `auto`, the fallback cascade is **docling CLI → docling-serve → Mistral OCR → disabled**. The chosen extractor is reported at startup and by `dir2mcp doctor` (e.g. `OCR: mistral-ocr (fallback; docling not found on PATH)`), so the active path is visible rather than inferred per document.

**Best-available *per format* (§7.4.B.1).** Selection is capability-aware: for each format, `auto` picks the highest-fidelity *active* engine that can actually read it, so no document is silently handed to an engine that can't (e.g. `.docx`/`.tiff` go to docling but are never routed to Mistral OCR, which can't import them), and a higher-fidelity engine is never bypassed. HTML is a dual-path format: when a structured engine (docling) is active it is routed there to preserve headings/tables/links (`extracted_markdown` with structured spans); otherwise it falls back to flat `raw_text` — so HTML is never dropped and never regresses when docling is absent.

**When no active engine covers a format** (`ingest.on_unsupported`, env `DIR2MCP_INGEST_ON_UNSUPPORTED`): `lenient` (default) skips the document with a warning and names the gap in the coverage report (`dir2mcp doctor`) — the backward-compatible, honest-not-silent outcome; `strict` records it as a non-fatal per-document `UNSUPPORTED_FORMAT` error (for CI / correctness-sensitive corpora). Either way the gap is surfaced, never silent.

An extractor counts as *available* only when it can actually **run**, not merely when it is configured (spec 0.15.0 §7.4). The `docling` CLI is functional-checked (a quick `docling --version` probe, cached for the run); a binary that is present but broken — e.g. a venv with ABI-incompatible dependencies — is treated as **unavailable**, exactly as an unreachable `serve_url` makes `docling-serve` unavailable. Under `auto` a broken docling is skipped and the cascade continues; under explicit `docling` it disables extraction (no silent fallback). `dir2mcp doctor` reports the real state instead of a false "healthy".

**Troubleshooting:**
- *`OCR: disabled`* — no extractor is available: install docling (or use the `-full` track), point `serve_url` at a docling-serve container, or set `MISTRAL_API_KEY`.
- *docling-serve rejected at startup (`CONFIG_INVALID`)* — `extractor: docling-serve` needs a non-empty, reachable `serve_url`; it never silently falls back to the CLI.
- *Switching extractors across re-indexes* is safe — docling and Mistral OCR both produce the same `extracted_markdown` representation; only the richness of span provenance (structured `region` spans vs. `page` spans) differs.
- *docling import errors / "two versions" of a Python package* — the docling CLI subprocess runs with a sanitized environment (`PYTHONPATH`/`PYTHONHOME` removed, `PYTHONNOUSERSITE=1`), so a conda install or stray `PYTHONPATH` in your shell can't shadow the bundled venv's pinned packages. With the `-full` track the venv is fully version-locked.
- *Expected docling but the banner shows `mistral-ocr (fallback ...)`* — docling either isn't on `PATH` or is present-but-broken (it failed the `docling --version` functional check, e.g. an ABI-incompatible venv). Under `auto` a non-functional docling is skipped and the cascade continues to docling-serve/Mistral; fix the install (or use the `-full` track), or pin `extractor: docling` to turn the broken install into a loud error instead of a silent fallback.
- *Wrong host's docling chosen / pointing at a custom binary* — set `ingest.docling.command` (`DIR2MCP_DOCLING_COMMAND`) to the command template; the resolved path is redacted from diagnostics. Confirm via the `extractor` row of `dir2mcp doctor` or `routing.json` in a support bundle.

#### Per-format mode keys: validated, no runtime effect yet

The config template also carries one mode key per format:

```yaml
ingest:
  pdf:
    mode: ocr          # off|ocr|auto
  images:
    mode: ocr_auto     # off|ocr_auto|ocr_on
  audio:
    mode: auto         # off|auto|on
  archives:
    mode: deep         # off|shallow|deep
```

Each of the four is a closed set. A value outside its set is rejected at startup
with `CONFIG_INVALID`, so `ingest.archives.mode: shalow` fails instead of loading as
if it had been understood. The value is also case-normalized, and an absent key keeps
the default above.

**No accepted value changes behavior yet.** The canonical spec lists the members of
each set without defining what any member does, so dir2mcp validates them and waits
for that definition rather than inventing one (see `dirstral-spec`). Until then, use
the keys that do work:

| Goal | Setting that works today |
|---|---|
| Turn PDF/image text extraction off | `ingest.extractor: off` |
| Choose the PDF/image engine | `ingest.extractor` (table above) |
| Turn audio/video transcription off | `stt.provider: off` |
| Report a format nothing can read | `ingest.on_unsupported: strict` |

You do not have to remember this. A generated config carries the same retraction as a
comment directly above the four keys. A hand-written `.dir2mcp.yaml` that sets one of
them to `off` also prints a startup warning. `off` gets the warning because it is the
value that costs money or privacy when it is wrong: the key withholds nothing, and the
warning names the key that does. The other values load silently, so a generated config
stays warning-free.

##### `ingest.archives.mode: deep` promises more than dir2mcp does

The default is the sharp case (issue #843). Archive handling today expands the **top
level** of an archive and nothing more:

| Value | What the name claims | What dir2mcp does |
|---|---|---|
| `off` | archives are not expanded | expands the top level anyway |
| `shallow` | top level only | top level only (a coincidence, not a read value) |
| `deep` (default) | a nested archive is expanded too | **no recursion at all** |

An archive nested inside an archive is not expanded. It is stored as a skipped
`archive_member` document with `skip_reason=archive`, so the container never reports
coverage it does not have and the gap appears in the coverage report.

The default stays `deep`, and no member changes meaning, because both come from the
canonical SPEC §16.2 template. A real `deep` implementation needs a recursion bound, a
byte budget for the expansion, and a defined outcome at the bound and on a cycle. The
spec defines none of those today, so that decision belongs in `dirstral-spec` first.

### docling extraction over HTTP (docling-serve)

Instead of spawning the docling CLI per document, dir2mcp can call a long-running [**docling-serve**](https://github.com/docling-project/docling-serve) HTTP container. This is the same docling engine and produces **byte-identical** output (the same structured `DoclingDocument` → Markdown + region citations) — only the transport differs (spec 0.10.0 §7.4.B).

```yaml
ingest:
  extractor: docling-serve         # or: auto
  docling:
    serve_url: http://127.0.0.1:5001
```

- Run the container yourself, e.g.: `docker run --rm -p 5001:5001 ghcr.io/docling-project/docling-serve-cpu`. dir2mcp does **not** start or stop it — lifecycle is user-managed.
- `extractor: docling-serve` **requires** a non-empty, reachable `serve_url`: an empty or unreachable endpoint is rejected at startup (`CONFIG_INVALID`). It never silently falls back to the docling CLI.
- `dir2mcp doctor` reports the same availability decision, so a dead `docling-serve` endpoint shows up as an unavailable extractor instead of surfacing only later as per-document runtime failures.
- Under `extractor: auto`, docling-serve is used only when the docling CLI isn't on `PATH` (local CLI is preferred); an empty `serve_url` means the HTTP transport isn't considered, and an unreachable one is skipped in favor of another available extractor (for example Mistral OCR).
- Env equivalent: `DIR2MCP_DOCLING_SERVE_URL=http://127.0.0.1:5001`.

### Extractor observability: which provider ran, and why

The extractor decision (which engine was chosen, and whether it was a fallback)
is surfaced consistently on three diagnostic surfaces — they all read the same
resolution logic, so they never disagree:

| Surface | How to see it | What it shows |
|---|---|---|
| Startup banner | printed by `dir2mcp up` in the **Models** section | `OCR: <provider> (<reason>)`, e.g. `OCR: docling (auto-detected on PATH)` or `OCR: mistral-ocr (fallback; docling not found on PATH; falling back to Mistral OCR)` |
| `dir2mcp doctor` | run against the daemon | an `extractor` check whose detail is `<provider> (<reason>)`; status is `ok` normally, **`warn`** when the choice is a fallback or when extraction is disabled |
| Support bundle | `dir2mcp support-bundle` → `routing.json` inside the tarball | machine-readable routing decisions (the same `provider` + `reason` data the banner shows), so a maintainer can confirm the backend without re-running `up` |

The decision carries three fields internally:

- **provider name** — `docling`, `docling-serve`, `mistral-ocr`, or empty (disabled);
- **source** — `explicit` (you pinned `ingest.extractor`), `auto` (auto-detected as the preferred path), `fallback` (preferred path unavailable), or `disabled`;
- **reason** — a human-readable explanation (e.g. `docling not found on PATH; falling back to Mistral OCR`). User-supplied docling command templates and resolved paths are intentionally **redacted** from the reason so no secret-bearing flag leaks into the banner or `routing.json`.

**Per-document extraction errors** are recorded in the optional **batch run
manifest** (a JSONL file, one record per asset, enabled with
`media.batch.manifest: <path>`). Each error record carries a canonical
`error_code` — `EXTRACT_FAILED` for a representation/derivation failure (this
covers OCR/docling extraction failures) or `TRANSCRIBE_FAILED` for a transcript
provider failure — plus a redacted `error_message`. Aggregate the manifest to
count failures per run.

> **Gap (no per-provider counters yet):** dir2mcp does **not** currently expose
> numeric counters/metrics broken down by extractor provider, fallback events,
> or error type. The only running numeric counter is an **aggregate**
> document-level error count in indexing status (it does not distinguish
> extraction failures from other errors, nor which provider produced them). For
> per-provider / per-error breakdowns today, parse the batch manifest. Finer
> error classification (e.g. `OCR_FAILED`) is also a tracked follow-up rather
> than something emitted today.

### Migration & rollout: adopting docling in stages

You can move from a lean, docling-free setup to local structured extraction (or
to a shared `docling-serve`) without a flag day — switching extractors across
re-indexes is safe because every engine produces the same `extracted_markdown`
representation (only span-provenance richness differs: structured `region` spans
vs. flat `page` spans). Suggested phases:

1. **Baseline (lean, no extraction or Mistral OCR).** Install
   `dir2mcp` (lean). With no docling on `PATH`, `extractor: auto` resolves to
   Mistral OCR when `MISTRAL_API_KEY` is set, else `disabled`. Confirm the
   active path on the `dir2mcp up` banner / `dir2mcp doctor` `extractor` check
   before indexing — a `disabled` extractor silently drops PDF/image text.
2. **Pilot docling on one host.** Either `brew install dirstral/tap/dir2mcp-full`
   (bundled, version-locked runtime) **or** install your own `docling` on
   `PATH`. Leave `extractor: auto`: docling is now preferred automatically and
   the banner should read `OCR: docling (...)`. Re-index a sample and spot-check
   region citations.
3. **Centralize via `docling-serve` (optional).** Stand up a long-running
   [docling-serve](#docling-extraction-over-http-docling-serve) container and
   point clients at it with `ingest.docling.serve_url`
   (`DIR2MCP_DOCLING_SERVE_URL`). Under `auto`, a host without the docling CLI
   uses the endpoint; pin `extractor: docling-serve` to require it (no silent
   fallback). Output is byte-identical to the CLI.
4. **Pin once you are confident.** Replace `auto` with an explicit
   `extractor: docling` (or `docling-serve`) so a host that loses its docling
   install **fails loudly** (extraction disabled) instead of silently degrading
   to Mistral OCR. Keep `auto` if a graceful Mistral fallback is what you want.

**Staged-enablement checklist:**

- [ ] Decide the track per host: lean (`dir2mcp`) if you bring docling /
      docling-serve / Mistral yourself, full (`dir2mcp-full`) for batteries-included local docling.
- [ ] Set `MISTRAL_API_KEY` if (and only if) you want a Mistral OCR fallback or use it as the primary extractor.
- [ ] After each change, verify the chosen extractor on the `up` banner or via
      `dir2mcp doctor` (watch for `warn`/`disabled`).
- [ ] Re-index a representative sample and confirm citations resolve as expected.
- [ ] Enable `media.batch.manifest` during the rollout to capture per-asset
      `EXTRACT_FAILED` / `TRANSCRIBE_FAILED` records, then review the manifest.
- [ ] Once stable, consider pinning `ingest.extractor` (drop `auto`) so missing
      docling is a loud error rather than a silent fallback.

For a multi-host / GPU-VPS topology (self-hosted extractor + remote corpus), see
[docs/dual-machine-deployment.md](docs/dual-machine-deployment.md).

### Fully local / no-egress setup

The default quickstart (and `.env.example`) configures **cloud** providers, so a corpus is processed by third-party APIs (Mistral for embeddings/OCR/STT/generation, ElevenLabs for voice). If your data must **not leave the host** (data-residency / compliance / on-prem archives), configure every capability against endpoints you run — dir2mcp treats a self-hosted provider as first-class (SPEC §8.5) and needs no cloud key.

Provide **no** cloud credentials (do not set `MISTRAL_API_KEY`, `ELEVENLABS_API_KEY`, etc. — with none present, auto-selection has nothing cloud to pick) and bind each capability explicitly in `.dir2mcp.yaml`:

```yaml
# .dir2mcp.yaml — fully local, no egress
providers:
  local-llm:                              # OpenAI-compatible server you run
    kind: openai                          #   (Ollama, vLLM, llama.cpp, LM Studio, TEI, …)
    base_url: http://127.0.0.1:11434/v1   # e.g. Ollama's OpenAI-compatible endpoint
    embed_text_model: nomic-embed-text
    embed_code_model: nomic-embed-text
    chat_model: llama3.1                  # answers + translation stay local
  local-stt:                              # self-hosted Whisper/WhisperX
    kind: whisper                         # base_url is the host ROOT (/v1/audio/transcriptions is appended)
    base_url: http://127.0.0.1:9001
    stt_model: large-v3
model:
  embed:
    provider: local-llm                   # reindex-bound (the embed identity includes it)
  chat:
    provider: local-llm
stt_provider: local-stt                   # STT uses the legacy selector
ingest:
  extractor: docling                      # local structured extraction; do NOT fall back to cloud OCR
```

Notes:
- **Document extraction:** use local `docling` (the `dir2mcp-full` track bundles it) or a self-hosted [docling-serve](#docling-extraction-over-http-docling-serve). Avoid `extractor: auto`, whose last fallback is cloud Mistral OCR — pin `extractor: docling` (or `docling-serve`, or `off`) so no page image is ever uploaded. For a self-hosted OCR endpoint instead, bind `model.ocr.provider` to a `kind: mistral` `/v1/ocr` profile (see below).
- **Verify, don't infer:** run `dir2mcp doctor` — its **egress** row must report `no third-party egress: all resolved providers target local/loopback or private/LAN endpoints`. If it names any public host, that capability is still leaving the machine.
- A trusted-LAN endpoint may be **credential-less** (omit `api_key`); loopback, private-range, `.local`/`.internal`, and single-label LAN hosts all count as no-egress.
- For a multi-machine topology (corpus over NFS/S3, a GPU box on the LAN, systemd units), see [docs/dual-machine-deployment.md](docs/dual-machine-deployment.md) and the self-hosted provider contract below.

### Self-hosted / GPU-VPS provider endpoints (embed / OCR / STT)

A self-hosted model server (on a GPU VPS or a trusted LAN) is a first-class provider: declare it under `providers:` with a custom `base_url` and bind it per capability (spec §8.5). No new provider `kind` is introduced, and a trusted-network endpoint may be **credential-less** (no `api_key`).

```yaml
providers:
  gpu-embed:                 # OpenAI-compatible embed/chat (TEI, vLLM, Infinity, …)
    kind: openai
    base_url: http://gpu-vps:8080/v1
    embed_text_model: bge-m3
  whisper:                   # self-hosted STT (POST {base_url}/v1/audio/transcriptions)
    kind: whisper            # base_url is the host ROOT; /v1/audio/transcriptions is appended
    base_url: http://gpu-vps:9001
    stt_model: large-v3
  gpu-ocr:                   # self-hosted bespoke OCR (POST {base_url}/v1/ocr)
    kind: mistral            # base_url is the host ROOT; /v1/ocr is appended
    base_url: http://gpu-vps:9100
    ocr_model: my-ocr
model:
  embed:
    provider: gpu-embed      # reindex-bound (the embed identity includes it)
  ocr:
    provider: gpu-ocr        # omit to keep the hosted mistral-ocr default
stt_provider: whisper        # STT uses the legacy selector
```

- **Capability mapping** (which route serves each capability, spec §8.5): embed → `POST {base_url}/v1/embeddings`; chat → `/v1/chat/completions`; STT → `/v1/audio/transcriptions` (endpoint-dependent, validated at first use). **OCR has no OpenAI analog** — bind it only to a `kind: mistral` `/v1/ocr` endpoint (or use [docling-serve](#docling-extraction-over-http-docling-serve)); binding `model.ocr.provider` to a `kind: openai` profile is rejected as `CONFIG_INVALID`.
- **`base_url` shape differs by kind:** a `kind: openai` `base_url` already includes `/v1`; a `kind: mistral` OCR `base_url` and a `kind: whisper` STT `base_url` are the **host root** (the client appends `/v1/ocr` and `/v1/audio/transcriptions` respectively). The whisper client tolerates a stray trailing `/v1` and will not double it.
- No shipped self-hosted defaults — you must declare the profile and bind it explicitly; nothing silently auto-selects a self-hosted endpoint.
- For a full GPU-VPS topology (corpus over NFS/S3, vector backend, systemd), see [docs/dual-machine-deployment.md](docs/dual-machine-deployment.md).

### Continuous incremental indexing (optional)

By default `dir2mcp up` scans the directory once at startup. Enable the **filesystem watcher** to keep the index continuously in sync with on-disk changes for the life of the process — added files are indexed, edited files are re-indexed, and removed files are tombstoned (evicted from retrieval).

```yaml
ingest:
  watch: true            # default: false
  watch_debounce: 500ms  # coalesce editor write bursts before re-indexing
```

Notes:

- The watcher runs alongside the existing embedding worker, so newly indexed files become searchable automatically without a manual `reindex`.
- It is **best-effort, not a correctness guarantee**: a low-frequency safety rescan reconciles anything missed (kernel event coalescing, OS watch limits on very large trees), so the index converges even if individual events are dropped.
- A **directory** that is removed, or renamed out of the corpus, retires every document below it at once. A filesystem reports one event for the directory, not one per file, so the watcher reconciles the descendants itself instead of leaving them searchable until the next safety rescan.
- A **large burst** can fill the watcher's internal job queue while one document is being indexed. A change the queue cannot take is dropped on purpose, because blocking there is what makes the kernel drop events, so the drop asks for an immediate reconcile instead and is written to the log. The corpus converges on that reconcile, not on the next periodic rescan.
- Excluded paths, `.gitignore` rules, and size/type limits apply to watched changes exactly as they do to the initial scan.
- A file that **stops** being eligible is retired at once. If it grows past `ingest.max_file_mb` it keeps a visible `skipped` row with the reason, and its chunks leave retrieval. If it becomes gitignored it is tombstoned, exactly as a full rescan would tombstone it. An edit to a `.gitignore` file triggers a reconcile of the tree, because one rule can change the eligibility of many paths at once.
- **The watcher needs a filesystem.** `source.kind: local` and `source.kind: nfs` are ordinary directory trees, so both use it. A remote corpus (`source.kind: s3`) has no filesystem to watch, so the watcher does not start for it. The index reconciles on a periodic rescan of the remote source instead, and `dir2mcp up` prints a warning at startup. `watch_debounce` and the `watch_overflows` stat apply only to the filesystem watcher; a remote corpus reports neither.
- Env equivalents: `DIR2MCP_INGEST_WATCH=true`, `DIR2MCP_INGEST_WATCH_DEBOUNCE=500ms`.

### Gemini embeddings (`gemini-embedding-001`)

dir2mcp can use Google's [Gemini Embedding model](https://deepmind.google/models/gemini/embedding/) (`gemini-embedding-001`) at full parity via Gemini's **native** embed API. Set `GEMINI_API_KEY` and bind embeddings to the `gemini` provider (auto-selection still prefers Mistral, so the binding is explicit):

```yaml
model:
  embed:                       # reindex-bound — see note below
    provider: gemini
    text_model: gemini-embedding-001
    code_model: gemini-embedding-001
    # Optional Matryoshka output dimensionality (native 3072; truncatable
    # to 1536 / 768). Omit for the native dimension. Truncated vectors are
    # re-normalized automatically.
    text_dim: 3072
    code_dim: 3072
```

- **Asymmetric `taskType`** (better retrieval quality): the document/query role is mapped automatically — corpus content embeds as `RETRIEVAL_DOCUMENT`, search queries as `RETRIEVAL_QUERY`, and a query against the configured `code_model` as `CODE_RETRIEVAL_QUERY`. No configuration needed; the role is set by the call site.
- **Matryoshka dimensions** (`text_dim`/`code_dim`): request a smaller vector to shrink the index. dir2mcp sends Gemini's `outputDimensionality` and L2-normalizes the truncated vectors. The knob is Gemini-only — setting it on a provider that can't honor it is rejected at startup (`CONFIG_INVALID`).
- **Reindex-bound (spec §8.1.4/§8.1.6):** the embed provider, model, **and requested dimension** form the corpus-lifetime embed identity. Switching to Gemini, or changing the dimension later, requires a `dir2mcp reindex` (the server refuses to mix vector spaces and tells you to reindex).

#### Multimodal embeddings (`gemini-embedding-2`, preview — implemented, default-off)

Spec §8.1.7 (0.14.0) defines an opt-in `model.embed.multimodal` mode (`off` default | `augment` | `replace`) that embeds media directly into the same vector space via the multimodal `gemini-embedding-2` model. It is being implemented in phases against a **Public Preview** model:

```yaml
model:
  embed:
    provider: gemini
    text_model: gemini-embedding-2
    code_model: gemini-embedding-2
    multimodal: augment      # off (default) | augment | replace
```

- **Images**, **PDFs**, **audio**, and **video** are supported today. Under `augment` the document is indexed *both* as text (image OCR / docling PDF text / audio transcript, if configured) *and* as direct media embeddings; under `replace` it is embedded directly *instead of* text. A PDF is embedded **per page** (each page is its own vector with a page citation); audio and video are embedded **per time window** (each window is its own vector with a `start_ms`/`end_ms` citation). A text query then retrieves images, PDF pages, and media windows from the shared space.
- **Audio/video duration probing and window extraction** use the external `ffprobe`/`ffmpeg` binaries. When they are absent, audio/video are skipped for direct embedding and the file keeps its text path (audio transcript) — a graceful fallback, not an error. Direct audio embedding covers MP3/WAV; video covers MP4/MOV (the formats the preview model accepts). Other audio formats keep only their transcript; video has no text path.
- `augment`/`replace` require `provider: gemini` with `text_model` and `code_model` both `gemini-embedding-2` (validated at startup; otherwise `CONFIG_INVALID`), and the mode is reindex-bound (§8.1.4). `off` (default) is fully behavior-preserving. The mode is **YAML-only** (`model.embed.multimodal`); there is no environment-variable override, since it is a deploy-time, reindex-bound choice.
- **Retrieval & citations.** Media hits surface in `search`/`ask` results with their `modality` and `media_ref`. To avoid double-counting, a coarse page-image candidate is dropped when a text/region candidate survives for the same `(rel_path, page)`. `ask` grounds on available text (an `augment` hit's OCR/transcript); a `replace`-mode media-only hit is cited without quoted context. `open_file` on a media-only chunk returns the non-retryable `MEDIA_NO_TEXT` (never raw bytes), distinct from the retryable `OCR_NOT_READY` when text is merely pending.

See [Design 0003](dirstral-spec/docs/design/0003-multimodal-embeddings.md).

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

### Relevance floor and insufficient-evidence abstention

Two separate controls decide what counts as evidence (SPEC §9.4.3). Both are **server-side**: neither is an MCP tool parameter, so neither changes a tool input/output schema.

**1. The relevance floor (`retrieval.min_score`) is a RELATIVE pruning control.** It drops low-scoring candidate hits before they reach the model, so a query with no strongly-relevant chunks returns fewer results instead of diluting the answer with weak context.

- The floor is applied after scoring/fusion, reranking, the fusion's own dedup/truncation, and recency decay, and compares each hit's final score as a **ratio to the best-scoring hit of the same result set**. It runs on the candidate pool **before** the final truncation to the caller's `k`, deliberately, so a survivor that pure relevance ranked at `k+1..k+n` can still surface (#427). A consequence worth knowing when reading query logs: while the pool is larger than `k`, pruning a hit changes nothing the caller sees, because truncation would have cut that hit anyway. The floor becomes visible exactly when the surviving pool is at or below `k`, which is the narrow or filtered query. Raw scores are incommensurable across retrieval modes (cosine ≈ `0..1`, RRF max ≈ `0.033`, a provider-specific rerank scale), so a ratio is what makes one configured number mean the same thing in every mode: it is unchanged when the whole set is rescaled. A hit whose ratio equals the floor is **kept** (strict less-than drops).
- It **ships enabled** at `0.05`, i.e. "drop a hit that scores below 5% of the best hit". A set of near-equal candidates therefore survives in full, and a hit reaches `0` only when its own score is `0`. Set `0` to disable it explicitly. Values outside `[0,1]` are rejected as invalid config.
- When the result set has no finite positive best score (every score is `0`, all of them are negative, all of them are `NaN`, or the best one is `+Inf`), no ratio is defined and the floor keeps the set intact rather than pruning on a meaningless comparison. A single unreadable hit (`NaN`) beside readable ones is the opposite case, and it drops.
- Being relative, this floor **cannot** express "the best hit is too weak": the top hit is `1.0` by construction, so some hit always clears any floor. That is the job of the second control.

  ```yaml
  retrieval:
    min_score: 0.05   # ships enabled; 0 disables; 1 keeps only the top-scoring hit(s)
  ```

**2. Abstention uses an ABSOLUTE evidence threshold.** When retrieval returns candidates but none of them is strong enough on an absolute scale, `ask` does not generate an answer from them: it returns an explicit *insufficient evidence* answer with an **empty `citations` array** (a normal result, not an error), and keeps the rejected candidates in `hits` so you can see what was turned down. Its wording differs from the empty-corpus answer, so a caller can tell "I found nothing" apart from "I found material and judged it too weak".

- **Signal and scale:** the hit's own `(query, chunk)` score, tagged with the scale it is on. `cosine` is the vector index's query/chunk cosine similarity; `rerank` is the reranker's relevance score for the pair. A candidate that reached the result set only through lexical BM25 carries no absolute signal (an FTS5 `bm25()` score is corpus-relative, and an RRF score encodes rank rather than relevance).
- **Shipped values:** `cosine ≥ 0.05`, `rerank ≥ 0.02`. They are deliberately conservative, because embedding cosine baselines are provider-dependent and a tighter default would make the guard silently corpus-specific. They are server constants and are **not** operator-configurable; `retrieval.min_score` configures the pruning floor only.
- **Aggregation:** the eligible set clears the threshold when its **strongest** hit does, each hit measured against the threshold for its **own** scale (one response may legitimately carry several scales at once).
- **Blind spot:** when no eligible hit carries an absolute signal at all, the guard fails **open** and the answer is generated. Suppressing answers on a corpus whose vector index is simply unavailable would be the worse failure.

An optional **recency time-decay** boosts newer content for dated corpora (news, logs, changelogs, meeting notes). It is **server-side and config-only** — not an MCP tool parameter, so it changes no tool input/output schema.

- When configured, each hit's final score is multiplied by an exponential decay `exp(-ln2 * age / half_life)`, where `age` is the hit's source-document mtime relative to a fixed "now" captured at query start. The decay is applied just **before** the relevance floor; re-scored hits are re-sorted deterministically.
- A hit whose date cannot be resolved is **never boosted nor penalized**, and a future-dated hit (clock skew) is clamped so it cannot be amplified above its raw score.
- **Default empty/`0` = disabled** (pass-through): behavior is unchanged unless you configure it. A negative half-life is rejected as invalid config.

  ```yaml
  retrieval:
    recency_half_life: 0   # 0/empty disables; e.g. 720h halves a 30-day-old hit's score
  ```

### HyDE query transform (optional)

**HyDE** (Hypothetical Document Embeddings) generates a short hypothetical answer to the query, embeds *that*, and retrieves with it — often improving recall for terse or keyword-style queries by closing the query↔document style gap. It is **server-side and config-only** (not an MCP tool parameter), so it changes no tool input/output schema.

- **Default off**: behavior is unchanged unless you enable it. Enabling it adds one generation call per search to produce the hypothetical answer.
- **Graceful degradation**: a generation failure (or no configured generator) falls back to the raw query — HyDE is an optimization, never a hard dependency.
- **Modes**:
  - `fuse` (default): RRF-fuses the hypothetical-document hits with the raw-query hits.
  - `replace`: retrieves with the hypothetical-document embedding alone.

  ```yaml
  retrieval:
    hyde:
      enabled: false   # set true to opt in
      mode: fuse       # fuse (default) | replace
  ```

### Hierarchical (coarse-to-fine) retrieval (optional)

For long documents and long media the answer often needs document-level context that no single chunk carries. **Hierarchical retrieval** (the RAPTOR / parent-document technique) derives a short model-generated **`summary` representation** per document, embeds it alongside that document's fine chunks, and — when a summary matches the query — **expands it to the fine chunks beneath it** before dedup/rerank. It is **server-side and config-only** (not an MCP tool parameter), so it changes no tool input/output schema.

- **Summaries retrieve, chunks cite.** A summary is a routing device: it is never returned as a hit, never a citation snippet, and never an answer quote. Every citation still points at a real source span in a fine chunk.
- **Default off**, and **not a reindex**: a summary is an *additive* vector in the same embedding space as its document's chunks, so toggling this adds/removes summary vectors and never re-embeds the corpus.
- **Capability-driven and fail-open**: summaries are generated by the configured chat provider (one call per document). With no chat provider — or on a per-document generation failure — that document simply has no summary and falls back to flat retrieval; the next scan retries.
- **Cost**: one bounded generation call per document, cached by derivation identity, so an unchanged document is not re-summarized.
- **Scope today**: document-level summaries. Section/event-level windowed summaries (`levels: [section]`, `section_units` / `section_seconds`) are a follow-up; configuring `section` today logs a warning and derives document-level summaries only.

  ```yaml
  retrieval:
    hierarchical:
      enabled: false        # set true to opt in
      source_reps: auto     # auto (default) = the document's primary retrievable text
                            #   representation; or an explicit list, e.g.
                            #   [extracted_markdown, transcript]
      levels: [document]    # document (default) | section (not implemented yet)
      provider: ""          # optional generator profile; empty => the configured chat provider
      max_tokens: 512       # per-summary generation bound
      prompt_version: v1    # names the built-in, domain-free template
      # prompt: ""          # optional override; the effective prompt is hashed into the
                            #   summary derivation identity, so an edit re-derives summaries
  ```

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
- The MCP clients (the `ask`/`search`/`open-file`/`list-files` shims and the
  ElevenLabs bridge) buffer at most 64 MiB of one upstream response. A larger
  response fails with an error that names the limit. The client never proxies
  the oversized body
- These MCP clients do not follow HTTP redirects. A 3xx from the endpoint is
  reported as a failure, so connection headers stay on the configured host
- Every provider adapter (Anthropic, Cohere, ColBERT, ElevenLabs, Gemini,
  Mistral, OmniEmbed, OpenAI, Whisper API) uses one shared HTTP path. It caps a
  successful JSON response at 64 MiB and a successful audio response at
  256 MiB. A larger response fails with an error that names the limit, so a
  hostile or broken endpoint cannot exhaust memory
- Provider adapters do not follow HTTP redirects either. Go keeps a custom
  API-key header (`x-api-key`, `xi-api-key`, `x-goog-api-key`) on a redirect to
  another host, so a 3xx from a provider endpoint is reported as a failure and
  the key stays on the configured host

### What a support bundle discloses

`dir2mcp support-bundle` is meant to be pasted into a public issue, so it is
filtered on two independent tiers:

| Tier | What it covers | When it is removed |
|---|---|---|
| Credentials | bearer tokens, `Authorization` headers, the `user:pass@` userinfo of any URL, and the value of every URL query/fragment parameter | **always**, in every mode |
| Local environment | corpus paths and titles, extraction error messages, and the config snapshot's paths, bind addresses, endpoints, prompts and operator-written glob/regex/word lists | by default; kept with `--include-content` |

`--include-content` widens the second tier only. It never re-enables credential
disclosure.

What **remains** in a default bundle: the version and OS, the routing decisions
(`routing.json`), and every closed-domain config setting — booleans, numbers,
durations, enums, and provider/model names. That is the material a maintainer
actually triages against. Removed values are marked `"[redacted]"` in
`config.snapshot.yaml`, so an empty value still means "never configured" and the
two cases stay distinguishable; a removed list also keeps its item count.

The archive is written owner-only (`0600`) and atomically, so a failed run
cannot leave a truncated or world-readable bundle behind. Redaction applies to
the bundle's copy only — the snapshot on disk under the state directory is
never rewritten.

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
- [dirstral-spec/spec/versioning.md](dirstral-spec/spec/versioning.md) — spec versioning policy and the implementation **compatibility matrix** (the canonical record of which spec version dir2mcp targets; the vendored submodule carries the current spec version in its header)

Operator guides (in-repo):

- [docs/dual-machine-deployment.md](docs/dual-machine-deployment.md) — run dir2mcp on a GPU VPS with the corpus on NFS or S3

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
`make cyclo` runs the cyclomatic-complexity gate used by CI. Code quality is enforced by `make lint` (golangci-lint, 0 issues) in CI — the README badge links to golangci-lint, the successor recommended by Go Report Card after it was sunset in 2026.

Contributor and agent guides: [AGENTS.md](AGENTS.md) · [CLAUDE.md](CLAUDE.md)

## License

MIT. See [LICENSE](LICENSE).
