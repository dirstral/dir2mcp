# VISION.md

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

For true air-gapped mode, embeddings/OCR/transcription must run via local/on-prem connectors deployed inside the same air-gapped network boundary. Local/on-prem connectors implement the runtime interfaces `embed(text) -> vector`, `ocr(image) -> text`, and `transcribe(audio) -> text`; if required connectors are unavailable, the runtime must fall back to text-first retrieval/citation mode and disable OCR/transcription-dependent ingestion paths.

Text-first retrieval/citation mode means retrieval is limited to existing text content already available to the index—plain text/code/docs, PDFs' extracted text if already present, and previously stored transcripts/OCR text—and embeddings are generated through `embed(text)` when available. When `embed(text) -> vector` is unavailable, the fallback ranking algorithm is deterministic BM25 (k1=1.5, b=0.75) with tokenization by Unicode word boundaries and lowercase normalization. Stop-word handling is configurable: by default common stop words are removed using the standard **Lucene English** stop‑word list[^lucene], but implementations MAY instead choose “no stop words” when absolute determinism is required. OCR/transcription-dependent ingestion pipelines are disabled in this mode.

"Required connectors" means only the connector types needed for modalities present in the current corpus segment (not all three universally):
- `embed(text)` is required for vector retrieval on text content
- `ocr(image)` is required only when image/PDF OCR ingestion is needed
- `transcribe(audio)` is required only when audio ingestion is needed

Already-ingested OCR/transcription artifacts remain searchable during fallback, must be flagged as `derived`, and should be scheduled for re-validation once connector health is restored (optional outside regulated environments; required for use case #2). Entering fallback must emit a user-visible and logged warning with connector name(s) and timestamp for operator remediation.

See **Connector interface contract (local/on-prem)** below for the full implementation contract.

[^lucene]: The Lucene English stop‑word list is a widely used default set of roughly 33 short words
(e.g. “a”, “an”, “the”, “and”, “or”, “but”, “if”, etc.).  Implementations may substitute
another standard list (such as NLTK’s English stop words) or disable stop‑word
filtering entirely when determinism is paramount.  The canonical Lucene list is
published at <https://lucene.apache.org/core/analysis-common/8_11_0/org/apache/lucene/analysis/en/EnglishAnalyzer.html#stopwords>.

### 3) Agent sidecar for large repos
Agents navigate a repository via `search`, `open_file`, and citations—without reading everything.  
Use case: code understanding, onboarding, refactoring analysis.

### 4) Disposable research nodes
Download a corpus, deploy, query, delete. No workspace provisioning.

---

## Connector interface contract (local/on-prem)

Local/on-prem connectors are pluggable components running inside the regulated or air-gapped environment that provide model-backed ingestion services to dir2mcp without requiring external APIs.

Minimum contract:
- must expose `embed(text)`, `ocr(image)`, and/or `transcribe(audio)` interfaces
- must support both batch requests and streaming requests where applicable
- must return the standard metadata envelope defined in **Connector metadata envelope (JSON contract)**
- must support connector authentication in this preference order: mTLS, bearer token/API key, then HTTP Basic Auth (legacy only)
- must support secure credential storage (environment variables preferred; encrypted config files allowed); plaintext credentials in committed config are not allowed
- must support credential rotation without process restart
- must enforce TLS 1.2+ with certificate validation; self-signed certificates are disallowed in production by default — **exception**: air-gapped or regulated environments may use self-signed or internally-issued certificates provided they are validated against a configured trusted internal root CA (via certificate pinning or a configurable trusted CA bundle); this exception requires strict certificate validation (no hostname suppression, no skip-verify), documented key-rotation procedures, and explicit opt-in configuration; any use of internally-issued certificates must be documented in the deployment configuration
- must expose configurable model selection and resource limits (timeouts, concurrency, memory/throughput caps)
	- recommended defaults (baseline):
		- timeout: `embed=10s`, `ocr=60s`, `transcribe=120s`
		- per-connector concurrency limit: `8` in-flight requests
		- per-connector memory cap: `2 GiB`
		- batch size limit: `embed<=128` items, `ocr<=16` pages/images, `transcribe<=8` audio segments
		- per-connector rate limit: `120` requests/minute
- must emit deterministic error codes so the runtime can distinguish transient failures, auth/config failures, and capability unavailability

Connector discovery and configuration:
- connectors are loaded from explicit runtime config (`connectors` block in YAML/JSON) and may be overridden by environment variables for endpoint/auth values
- each connector definition must include transport endpoint details (HTTP/HTTPS URL, UNIX socket path, or gRPC endpoint), auth credentials/reference, supported capabilities, and allowed model selections
- startup sequence: read config → register each connector → perform handshake/capability validation (`embed(text)`, `ocr(image)`, `transcribe(audio)` as declared) → validate model/resource constraints → persist the connector metadata envelope for runtime selection and diagnostics
- if any startup step fails (for example: handshake timeout, capability mismatch, invalid model constraints), apply configured retries/timeouts, then log a structured error including connector name, endpoint, and error code; mark that connector unavailable; continue initializing remaining connectors; if all connectors fail, enter text-first degraded mode

Health-check and recovery protocol:
- connectors must expose health probes (`/health` for HTTP or gRPC HealthCheck equivalent) that return deterministic status/error codes
- runtime polls health at a configurable interval (`health_check_interval` in config, default `5s`) and on failure switches to bounded exponential backoff (max: `300s`) until a successful probe resets to the fixed interval
- on unhealthy state, disable affected ingestion paths and enter text-first retrieval/citation mode as needed
- on healthy restoration, re-enable ingestion paths and trigger re-validation of `derived` artifacts created during degraded operation

Re-validation mechanics and outcomes:
- on connector restoration, re-run the original ingestion operation on the original source artifact (OCR/transcription as applicable)
- perform deterministic comparison between newly generated output and stored `derived` artifact using connector/type-specific thresholds
- pass criteria: differences at or below configured threshold
- fail criteria: differences above threshold -> mark artifact `validation-failed`, flag for manual review, and update index only after explicit approval or documented automated reconciliation policy
- required logging for every re-validation: artifact path, connector id, timestamp, diff summary, threshold used, and reviewer/action taken
- threshold policy must be configurable per connector and artifact type; any automated index updates must be auditable

Runtime behavior requirements:
- when connector unavailability is detected from deterministic error codes, dir2mcp falls back to text-first retrieval/citation mode
- OCR/transcription-dependent ingestion paths are disabled until connector health is restored

Acceptable connector implementations include:
- local model runtimes on CPU/GPU hosts
- internal GPU inference servers
- vendor on-prem appliance endpoints

### Connector metadata envelope (JSON contract)

The metadata envelope is a JSON object returned by connector operations and handshake/health responses.

Required fields:
- `provider` (string): connector/provider identifier
- `model` (string): selected model identifier
- `version` (string): connector or model version string
- `latency_ms` (number; integer or float accepted): end-to-end latency in **milliseconds** (fractional values allowed to represent sub‑millisecond timings, e.g. `0.45`).  Implementations reporting sub‑ms precision should round or truncate consistently; values must be `>= 0`.
- `trace_or_request_id` (string): request correlation or request identifier.  Connectors **should** generate UUID‑v4 identifiers when possible (e.g. `550e8400-e29b-41d4-a716-446655440000`) but **may** generate arbitrary strings if a UUID cannot be generated (e.g. `req-12345`).  Consumers **must** accept non‑UUID identifiers and record them as provided but are encouraged to treat UUIDs specially for tracing and de‑duplication.  Connectors may log or normalize the generated value for internal purposes but must include the final identifier in the returned metadata envelope.
Optional fields:
- `token_or_compute_usage` (object): usage counters
	- `input_tokens` (number, `>= 0`) optional
	- `output_tokens` (number, `>= 0`) optional
	- `compute_ms` (number, `>= 0`) optional
	- `gpu_seconds` (number, `>= 0`) optional
	- `currency` (string) optional
	- `cost` (number, `>= 0`) optional

Null/omitted semantics:
- optional fields may be omitted when not available
- fields may be `null` only when explicitly unsupported by the connector; required fields must never be null

Canonical example:
```json
{
	"provider": "internal-gpu-inference",
	"model": "mistral-embed-v1",
	"version": "2026.02.1",
	"latency_ms": 47.3,
	"trace_or_request_id": "550e8400-e29b-41d4-a716-446655440000",
	"token_or_compute_usage": {
		"input_tokens": 312,
		"output_tokens": 0,
		"compute_ms": 45,
		"gpu_seconds": 0.02,
		"currency": "USD",
		"cost": 0.0003
	}
}
```

### Envelope-specific error codes

Codes specifically used by metadata envelope parsers; refer to parser
requirements earlier in this section for usage context.

- `ENVELOPE_MALFORMED_JSON (2003)` – JSON syntax error in metadata envelope.
- `ENVELOPE_REQUIRED_FIELD_MISSING (2004)` – required field absent.
- `ENVELOPE_REQUIRED_FIELD_TYPE_INVALID (2005)` – field present but wrong type.
- `ENVELOPE_CONSTRAINT_VIOLATION (2006)` – numeric constraint violation.

Parser requirements:
- connectors must return valid JSON for metadata envelopes; malformed JSON must produce structured connector errors (recommended: `ENVELOPE_MALFORMED_JSON (2003)`; see §"Envelope-specific error codes" above)
- missing required fields (`provider`, `model`, `version`, `latency_ms`, `trace_or_request_id`) must produce `ENVELOPE_REQUIRED_FIELD_MISSING (2004)` (see §"Envelope-specific error codes" above); `CONFIG_MISSING (2002)` is reserved for global connector configuration errors, not per-envelope missing fields
- required-field type mismatches (for example `latency_ms` not numeric, `provider` not string) must produce a structured connector error (recommended: `ENVELOPE_REQUIRED_FIELD_TYPE_INVALID (2005)`; see §"Envelope-specific error codes" above)
- numeric constraint violations (for example negative `latency_ms`, negative values in `token_or_compute_usage`) must produce validation errors (recommended: `ENVELOPE_CONSTRAINT_VIOLATION (2006)`; see §"Envelope-specific error codes" above)
- unexpected additional fields must be ignored for forward compatibility and must not fail parsing

### Connector error taxonomy (deterministic, machine-parseable)

Connector errors must use a structured object:

```json
{
	"code": 1001,
	"type": "NETWORK_TIMEOUT",
	"message": "Connector timed out while processing embed(text)"
}
```

Required fields:
- `code` (number): stable numeric error code
- `type` (string): machine-readable enum constant
- `message` (string): human-readable diagnostic summary

Reserved code ranges:
- `1000-1999`: transient/retryable failures
	- `NETWORK_TIMEOUT (1001)`
	- `UPSTREAM_UNAVAILABLE (1002)`
- `2000-2999`: auth/config failures
	- `AUTH_INVALID_CREDENTIALS (2001)`
	- `CONFIG_MISSING (2002)`

- `3000-3999`: capability unavailable failures
	- `CAPABILITY_NOT_SUPPORTED (3001)`
	- `MODEL_NOT_AVAILABLE (3002)`
- `4000-4999`: client/input failures
	- `INVALID_REQUEST (4001)`
	- `VALIDATION_FAILED (4002)`
- `5000-5999`: resource exhaustion/rate/quota failures
	- `RATE_LIMIT_EXCEEDED (5001)`
	- `QUOTA_EXCEEDED (5002)`
- `6000-6999`: internal/implementation failures
	- `INTERNAL_ERROR (6001)`
	- `PANIC_UNEXPECTED (6002)`

Canonical HTTP status mappings (explicit per-error recommendations):

Each named connector error constant is paired with a single “recommended” HTTP
status code.  The table below lists every constant defined in the taxonomy along
with its associated status; implementers should return the annotated status for
that error and may use the more general range guidance when new codes are
introduced.

| Error constant            | Code | HTTP status | Notes |
|---------------------------|------|-------------|-------|
| NETWORK_TIMEOUT           | 1001 | 503         | retryable/temporary |
| UPSTREAM_UNAVAILABLE      | 1002 | 502         | bad‑gateway connector fault |
| AUTH_INVALID_CREDENTIALS  | 2001 | 401         | invalid or missing authentication |
| CONFIG_MISSING            | 2002 | 503         | server-side config absent; service unavailable until corrected |
| CAPABILITY_NOT_SUPPORTED  | 3001 | 501         | unimplemented capability |
| MODEL_NOT_AVAILABLE       | 3002 | 422         | semantically unprocessable request |
| INVALID_REQUEST           | 4001 | 400         | malformed or invalid client input |
| VALIDATION_FAILED         | 4002 | 422         | well‑formed but semantically invalid input |
| RATE_LIMIT_EXCEEDED       | 5001 | 429         | rate/quota exhaustion |
| QUOTA_EXCEEDED            | 5002 | 429         | rate/quota exhaustion |
| INTERNAL_ERROR            | 6001 | 500         | internal server fault |
| PANIC_UNEXPECTED          | 6002 | 500         | internal server fault |

Generalized range guidelines (for new or unspecified codes):
- transient/retryable (`1000-1999`) → `503` for temporary overload/idempotent
  retry‑later conditions, `502` for upstream/bad‑gateway connector failures
- auth/config (`2000-2999`) → `401` for missing/invalid authentication, `403`
  for authenticated but forbidden/insufficient permissions or explicit policy
  denial, `503` for missing server-side configuration that makes the service
  unavailable (e.g. `CONFIG_MISSING`)
- capability unavailable (`3000-3999`) → `422` for semantically unprocessable
  requests, `501` for unimplemented/nonexistent capabilities
- client/input (`4000-4999`) → `400` for malformed/invalid client request, `422`
  for semantically invalid but well‑formed input
- resource exhaustion (`5000-5999`) → `429` for rate/quota exhaustion, `503`
  when capacity exhaustion is temporary service-side overload
- internal/implementation (`6000-6999`) → `500` for internal faults, `502`
  when fault is surfaced through an upstream connector boundary

Propagation requirements:
- connectors must return this structured error object in API error responses
- SDK wrappers/exceptions must preserve `code`, `type`, and `message` as machine-readable fields
- runtime and connector logs must emit the same structured error object for all surfaced failures

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
