# VISION.md

This document is intentionally product-level. Normative technical contracts and implementation details live in [SPEC.md](SPEC.md).

## dir2mcp: the deployment primitive for the agent web

### One sentence
`dir2mcp` turns a directory of privately hosted data into a **standard MCP tool server** in one command—so agents can connect immediately, and (optionally) access can be monetized via **HTTP-native payments (x402)**.

## Current state (March 2026)

- Core Go binary and MCP server runtime are implemented.
- Directory indexing + retrieval + citations are implemented and tested.
- Multimodal ingestion paths (OCR/transcription/annotation) and related MCP tools are available.
- Optional x402 request gating is implemented as facilitator-backed route protection for MCP `tools/call`.
- Active work focuses on release hardening and retrieval quality/completeness.
- Hosted demo smoke/runbook coverage now includes a scriptable MCP probe (`scripts/smoke_hosted_demo.sh`) for initialize/tools/list/tools-call readiness checks.

---

## The thesis

Agents are only as useful as the tools and knowledge they can reach. Most high-signal knowledge is private, messy, and distributed across machines people already control (repos, archives, PDFs, recordings, screenshots, docs, datasets, runbooks).

Today, exposing that knowledge to agents usually means either:
- uploading it to a SaaS,
- building and maintaining bespoke APIs, or
- giving the agent raw filesystem access (high risk).

The agent web becomes real when it’s as easy to deploy a **safe, verifiable, standard tool endpoint** as it is to run a web server.

`dir2mcp` is the “run a web server” moment for agent-accessible private knowledge.

---

## What we’re building

### The product
A single-binary CLI that:
1) scans and indexes a directory incrementally (fast, local-first),
2) **normalizes non-text into searchable text representations** (e.g., PDFs/images → OCR markdown; audio → transcripts; structured docs → extracted JSON + flattened text),
3) exposes the corpus as an **MCP server** with a small, stable tool surface,
4) prints connection info immediately so any MCP-capable agent can attach while indexing continues.

### The promise
- **One command:** install → run → connect.
- **Agent-native:** standard MCP tools, no bespoke client glue.
- **Safe by default:** local bind by default; explicit `--public` for network exposure.
- **Verifiable:** every answer cites file/line/page/time provenance.
- **Mistral-native (cloud or on-prem):** uses Mistral embeddings + OCR + transcription as first-class ingestion steps, with support for local/on-prem Mistral deployments for restricted environments.
- **Provider-flexible at the edges:** in fully air-gapped deployments, adapters can switch to local/on-prem providers (or disable unavailable modalities) while preserving the same retrieval/citation index contract.

---

## Why now

Two primitives are converging:
1) **Standard tool calling** (MCP) → interoperability between agents and services.
2) **Protocol-level payments** (x402 / HTTP 402) → machine-to-machine commerce without bespoke billing portals.

If deploying an MCP endpoint becomes trivial, and paid access can be enabled natively via x402 with a facilitator backend, we unlock an ecosystem where:
- individuals and teams expose specialized private knowledge as services,
- agents compose services dynamically,
- marketplaces index and monetize access with minimal friction.

---

## The long-term vision: knowledge microservices

### A directory becomes a service
A “knowledge microservice” is a small endpoint that provides:
- semantic retrieval over *all* corpus modalities (text/code/PDF/audio/images),
- optional RAG answering,
- source inspection tools,
- progress and metadata introspection.

`dir2mcp` makes this deployable from *any* directory in minutes.

### A service becomes a marketable resource (optional)
Once a knowledge service is a standard MCP endpoint, a marketplace can list it and a payment layer can meter it.

`dir2mcp` is not the marketplace. It is the deployable unit the marketplace can point to.

---

## Target users and “must win” use cases

### 1) Remote ephemeral knowledge node
SSH into a VPS, run `dir2mcp up --public`, connect an agent from your laptop.  
Use case: archived data, customer logs, large repos, one-off investigations.

### 2) Air-gapped / regulated environments
Local deployment with retrieval-only exposure, strong provenance, and no mandatory external API dependency.  
Use case: defense/healthcare/finance compliance constraints.

For true air-gapped mode, embeddings/OCR/transcription run through local/on-prem connectors inside the same trust boundary.

If a required connector for a given modality is unavailable, dir2mcp degrades gracefully:
- retrieval remains available over existing text representations,
- unavailable ingestion paths are paused until connector health is restored,
- operators get explicit warnings so degraded mode is visible.

See [SPEC.md](SPEC.md) for the exact connector contract and fallback semantics.

### 3) Agent sidecar for large repos
Agents navigate a repository via `search`, `open_file`, and citations—without reading everything.  
Use case: code understanding, onboarding, refactoring analysis.

### 4) Disposable research nodes
Download a corpus, deploy, query, delete. No workspace provisioning.

---

## Connector strategy (local/on-prem)

For regulated and air-gapped deployments, dir2mcp supports connector-based
operation so ingestion and retrieval can stay inside a controlled network.

At a vision level, the rule is simple:
- keep the same product behavior and MCP tool surface,
- swap model execution to local/on-prem connectors where required,
- degrade gracefully to text-first mode when specific modalities are unavailable.

This keeps the product promise stable across deployment environments without
turning the core project into a connector framework.

Implementation-level contracts (auth, health checks, metadata envelope,
error taxonomy, retries, re-validation policy) are intentionally specified in
the technical docs:
- [SPEC.md](SPEC.md) — normative contracts and wire/config behavior
- [ECOSYSTEM.md](ECOSYSTEM.md) — ecosystem/deployment positioning

## What makes dir2mcp different

Most “chat with docs” tools are:
- UI-first,
- centralized,
- heavy to deploy,
- not agent-native.

`dir2mcp` is:
- **deployment-first** (one binary, one command),
- **agent-native** (MCP tools),
- **local-first** (embedded index; no external DB),
- **multimodal-first** (OCR/transcription/structured extraction flow into the same RAG),
- **network-capable** (explicit public mode),
- **verifiable** (citations are first-class).

---

## Product principles

1) **One-command deployability**  
No additional services required; state lives in the directory.

2) **Fast time-to-first-answer**  
Server starts immediately; indexing continues in the background.

3) **Safe by default**  
Local bind default, explicit public mode, token auth, origin checks, strict root isolation.

4) **Minimal surface area**  
Small tool set; clear semantics; stable schemas.

5) **Reproducible and reversible**  
Delete `.dir2mcp/` and the service state is gone.

6) **Verifiable outputs**  
Citations always; provide `open_file` to inspect sources.

---

## The core tool surface

Minimum viable tools (stable, agent-friendly):
- `search(query, k, filters)` → ranked passages + provenance
- `ask(question, k, filters, mode)` → answer + citations (+ underlying hits)
- `open_file(path, range/page/time)` → exact source slice
- `list_files(glob/prefix)` → navigation
- `stats()` → progress, corpus profile, model info

Optional “deep” tools:
- `annotate(path, schema)` → structured extraction from a document + flattened text (for indexing)
- `transcribe(path)` → transcript segments (time-coded)
- `transcribe_and_ask(path, question)` → voice note → answer (no TTS required)

---

## Architecture: separation of concerns

### What dir2mcp owns
- file discovery and extraction
- OCR/transcription/structured extraction into text representations
- chunking and provenance metadata
- embeddings and embedded ANN indices
- retrieval and citation formatting
- MCP server and tool schemas
- safe network exposure defaults

### What dir2mcp can also own (optional paid extensions)
- optional, pluggable native x402 request-gating extension on selected MCP routes
- optional payment requirement declaration and price policy mapping per route/tool via extension configuration
- facilitator-backed verification and settlement integration through an explicit adapter contract (e.g., `x402 payment adapter`; see [x402 payment adapter spec](x402-payment-adapter-spec.md) for details). Payment state is owned by the facilitator layer. The adapter contract specifies the required HTTP endpoints/events, authentication mechanism, canonical payment state model, and standard error codes/retry semantics so implementers can locate and build against the `x402 payment adapter` interface.
- optional metering signal hooks for usage and payment analytics (exported to external systems)
- build/runtime feature flags to enable or disable payment extensions, shipping disabled by default (opt-in)

### What an external layer can own (later)
- marketplace discovery and trust metadata
- identity, attestations, and reputation
- portfolio-level billing analytics across many nodes

This separation keeps dir2mcp minimal and fast while enabling the broader infrastructure vision.

---

## Trust and safety (non-negotiables)

A marketplace of private knowledge endpoints only works if:
- providers can expose value without accidental leakage,
- consumers can verify outputs.

Baseline requirements:
- strict root isolation (no path traversal, no symlink escapes)
- `--public` must enable configurable rate limits by default (requests/minute + burst), with explicit override only
- retrieval-first outputs (no bulk export by default)
- provenance and inspection tools (`open_file`)
- secret-aware exclusions with concrete controls:
    - pattern-based regex detection for API keys/tokens.  The spec should either reference an established secret-detection source (e.g. the pattern sets used by [truffleHog](https://github.com/trufflesecurity/trufflehog) or [gitleaks](https://github.com/zricethezav/gitleaks)) or ship a small starter list of regular expressions for common secret types.  Example starter patterns might include:
        - AWS access keys: `AKIA[0-9A-Z]{16}` / `ASIA[0-9A-Z]{16}`
        - OAuth/JWT-like tokens: `[A-Za-z0-9\-_]{20,}`
        - Generic API keys: `(?i)(?:api[_-]?key)["'`]?\s*[:=]\s*[A-Za-z0-9\-_=]{16,}`
        - Private key headers: `-----BEGIN (?:RSA|EC|DSA) PRIVATE KEY-----`
      Implementers may extend, replace or override the provided patterns as needed; users must be able to supply their own regexes and completely disable the built‑in list if desired.
    - optional `.gitignore`-style path excludes (directories or filenames that should never be readable).
    - default exclusion patterns that users can override (e.g. `**/*.pem`, `**/*.env`, `**/node_modules/**`).
  `open_file` must honor all of these exclusions when serving content.
  Patterns and path filters are expected to be configurable at startup and modifiable via the API, so hosting software can layer additional rules.
- basic request logging (optional) for debugging and metering later
- optional immutable audit logging for regulated environments (accessor identity, timestamp, file/path, action), especially for use case #2.
- immutability mechanism: append-only `AuditService` emits to pluggable `AuditSink` backends (local file with cryptographic chaining/Merkle roots, external SIEM, or WORM storage).
- sink failure modes (`disk full`, `SIEM down`, `network partition`) are controlled by `audit.failMode = closed|open|queue`:
	- `closed`: reject requests requiring auditable actions
	- `open`: continue processing without audit persistence and emit high-severity alerts
	- `queue`: buffer records with bounded retry/backoff until sink recovers
- configuration knobs: `--audit` enables/disables audit subsystem, `audit.sink` selects sink implementation, `audit.queueSize` bounds queued records, and `audit.retryPolicy` controls retry count/backoff/jitter.
- rotation and retention controls:
	- `audit.rotation.strategy = size|time|hybrid`
	- `audit.rotation.size` max log size before rotation (recommended regulated default: `100MB`)
	- `audit.rotation.time` time-based rotation interval (recommended regulated default: `24h`)
	- `audit.retention.period` retention window before archival/deletion (recommended regulated minimum: `365d` unless policy requires longer)
	- `audit.archivePath` archive target (local immutable path or external object storage)
	- archival behavior should compress-and-move rotated segments to `audit.archivePath` (or sink-managed archival equivalent)
- queue/rotation interaction: if rotation or archival fails, queue mode must honor `audit.queueSize` and `audit.retryPolicy`; on queue exhaustion under `queue` mode, escalate to operator alert and apply configured overflow policy.
- secure deletion controls: `audit.secureDelete = off|overwrite|cryptographic_erasure`; regulated deployments should use `cryptographic_erasure` where supported, or `overwrite` when required by policy.
- cryptographic requirements:
	- Merkle root algorithm: SHA-256
	- per-record integrity: HMAC-SHA256 (keyed chaining) or ECDSA P-256 signatures
	- key management: keys generated/stored in KMS/HSM, offline root key for trust anchor, automated key rotation policy with key identifiers
	- output record fields must include at least: `record`, `timestamp`, `identity`, `signature_or_hmac`, `parent_hash`, `merkle_root`
	- verification tooling must support recomputing per-record HMAC/signatures and Merkle roots; provide CLI verification workflows for forensics and compliance checks. Example:

    ```bash
    dir2mcp audit verify --sink <sink> --from <ts> --to <ts>
    ```
- regulated deployments (use case #2) must default to `audit.failMode=closed`; any exception allowing `queue` requires documented policy approval, strict `audit.queueSize`/`audit.retryPolicy` limits, and explicit operational sign-off.
- prompt-injection posture: retrieved text is untrusted context, not instructions

---

## Roadmap

### Current baseline
- Go single binary
- embedded ANN + SQLite metadata
- incremental indexing (hash-based) + tombstones + oversampling filter
- MCP Streamable HTTP server, minimal tools
- citations with file/line/page/time provenance
- Mistral: embeddings + OCR + transcription flow into RAG (default)
- optional STT provider (e.g., ElevenLabs) via adapter without changing indexing model
- optional native x402 paid mode using facilitator integration for selected MCP routes

### Near-term roadmap
- compact/rebuild command for index hygiene
- file watch mode for “live” corpora
- better format coverage and caching policies
- endpoint manifest (capabilities + pricing hints + trust metadata)

### Longer-term (agent web infrastructure)
- delegated x402 integration hardening (pricing policies, route policies, richer metering) via facilitator adapters
- standardized metering hooks (per tool call, per token, per byte) exported to external facilitator/billing systems
- attestation of endpoint identity (signing)
- marketplace integration as a separate layer

---

## Non-goals (by design)
- becoming a general-purpose agent framework
- building a UI-first “chat app”
- running a centralized hosted platform
- building a custom custodial billing platform or exchange

---

## The end state
A world where:
- deploying private knowledge for agents is as easy as deploying a web server,
- services compose through MCP,
- optional x402-compatible payment gating, delegated to external facilitators, makes it feasible to buy/sell access safely and programmatically.

`dir2mcp` is the deployable unit that makes that ecosystem possible.
