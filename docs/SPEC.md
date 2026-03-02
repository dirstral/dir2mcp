# SPEC.md
## dir2mcp Output & Integration Specification (Go)

**Spec version:** `0.4.0`  
**MCP protocol target:** `2025-11-25` (Streamable HTTP transport, sessions, tools, structured tool output)  
**Primary goal:** one-command “deploy-now” directory RAG exposed as an **MCP Streamable HTTP** server, with an embedded on-disk index (**no external DB; no Qdrant**) and a single config file.  
**Implementation goal:** maximize Mistral capability utilization by flowing OCR + transcription + optional structured extraction into the same RAG pipeline, while allowing optional alternate providers where adapters exist.  
**Scope note:** x402 support is optional and additive; retrieval and MCP interoperability remain first-class regardless of payment mode.

---

## 0) Executive summary

`dir2mcp up` in any directory will:

1) Start an MCP server immediately (connect right away).  
2) Index the directory in the background (incremental, safe-by-default).  
3) Normalize non-text files into text representations:
   - PDFs/images → OCR markdown
   - audio → transcripts (STT provider configurable)
   - structured extraction → JSON + flattened text (on-demand by default)
4) Provide a small MCP tool surface for agents:
   - search, ask, open_file, list_files, stats
   - plus (recommended) transcribe, annotate, transcribe_and_ask
5) Optionally enable native x402 on selected routes/tools:
  - return HTTP 402 with payment requirements for unpaid requests (`PAYMENT-REQUIRED`)
  - accept client payment proofs via `PAYMENT-SIGNATURE`
  - verify/settle via configured facilitator
  - include settlement receipt metadata via `PAYMENT-RESPONSE` on successful paid calls
  - continue serving standard MCP responses after successful payment

### 0.1 External normative references for x402 mode

When `x402` mode is enabled, implementations SHOULD align with these references:

- x402 v2 specification: <https://github.com/coinbase/x402/tree/main/specs>
- CDP x402 facilitator API reference (`/v2/x402/verify`, `/v2/x402/settle`): <https://docs.cdp.coinbase.com/api-reference/v2/rest-api/x402-facilitator/x402-facilitator>
- x402 core flow and headers (`PAYMENT-REQUIRED`, `PAYMENT-SIGNATURE`, `PAYMENT-RESPONSE`):
  - <https://docs.cdp.coinbase.com/x402/core-concepts/how-it-works>
  - <https://docs.cdp.coinbase.com/x402/core-concepts/http-402>
  - <https://docs.cdp.coinbase.com/x402/migration-guide>
- Network and facilitator support (CAIP-2 identifiers): <https://docs.cdp.coinbase.com/x402/network-support>
- Bazaar discovery extension model: <https://docs.cdp.coinbase.com/x402/bazaar>

### 0.2 Implementation status notes (March 2026)

Status tags used in this spec:

- **Implemented:** available in current repository/runtime behavior.
- **Partially implemented:** interface exists, but not all target behavior is complete.
- **In progress:** work underway but not yet complete (may overlap with "partially implemented").
- **Planned:** target behavior not yet fully implemented.

Current high-level status:

- CLI + MCP server lifecycle, indexing pipeline, and core tool surface: **Implemented**
- Multimodal ingestion (OCR/transcription/annotation) and retrieval workflows: **Implemented** (with ongoing quality/perf hardening)
- Retrieval `Stats()` service contract: **Planned** (see issue #71)
- Advanced retrieval answer quality/completion work: **In progress** (see issue #70)
- Native x402 tools/call gating path: **Implemented** (optional and facilitator-backed)
- Hosted smoke/runbook guidance: **Implemented** (see issue #19)
- Release-completion checklist hardening: **In progress** (see issue #12)

---

## 1) Definitions and invariants

### 1.1 Terms
- **Root directory**: directory being indexed.
- **State directory**: storage location for index state (default: `<root>/.dir2mcp/`).
- **Document**: ingestible unit (file or archive member).
- **Representation (rep)**: a text view derived from a document:
  - `raw_text` (code/text/md/data/html converted to text)
  - `ocr_markdown` (OCR output for PDFs/images)
  - `transcript` (STT output for audio)
  - `annotation_json` (structured JSON result)
  - `annotation_text` (flattened `key: value` text derived from annotation_json)
- **Chunk**: span of a representation used for embedding and retrieval.
- **Span**: provenance coordinates for citations: line range, page number, or time range.

### 1.2 Invariants
- The MCP server accepts lifecycle requests immediately after `dir2mcp up` prints the endpoint URL.
- Indexing continues in the background; tools operate on partial index if needed.
- No content outside root is accessible via tools (no path traversal; no symlink escape).
- No external vector database is required or supported.

---

## 2) CLI interface

### 2.1 Commands
- `dir2mcp up`  
  Start MCP server and run indexing (incremental) in background.

- `dir2mcp status`  
  Read state from disk and show progress.

- `dir2mcp ask "QUESTION"`  
  Local convenience: runs RAG via the same engine (no MCP).

- `dir2mcp reindex`  
  Force full rebuild.

- `dir2mcp config init`  
  Interactive setup wizard (TTY default) that creates/updates `.dir2mcp.yaml` and configures secret sources.

- `dir2mcp config print`  
  Print effective config (defaults + file + env + flags).

- `dir2mcp version`

### 2.2 Global flags
- `--dir <path>`: root directory (default `.`)
- `--config <path>`: config file path (default: `./.dir2mcp.yaml`)
- `--state-dir <path>`: state directory (default: `<root>/.dir2mcp`)
- `--json`: NDJSON events for automation/logging
- `--non-interactive`: disable prompts; fail fast with actionable config instructions when required values are missing
- `--quiet`

### 2.3 `up` flags
- `--listen <host:port>` (default `127.0.0.1:0`)
- `--mcp-path <path>` (default `/mcp`)
- `--public` (shortcut: bind `0.0.0.0` and require token)
- `--auth auto|none|file:<path>`  
  Warning: do not pass bearer tokens on the command line—see Section 17 for secure token handling.
- `--tls-cert <path> --tls-key <path>`
- `--x402 off|on|required` (default `off`)
- `--x402-facilitator-url <url>`
- `--x402-resource-base-url <url>` (public base URL used in payment requirements)
- `--x402-network <network-id>` (e.g., `eip155:8453`)
- `--x402-price <value>` (default per-call price for paid routes)
- `--read-only` (dir2mcp is read-only by design; this hardens future additions)

### 2.4 Exit codes
- `0` success
- `1` generic error
- `2` config invalid
- `3` ingestion error (fatal; per-file errors remain non-fatal)
- `4` server startup error (bind/listen/runtime startup failure)
- `5` auth/payment error
- `6` signal/interrupt

### 2.5 Startup interaction contract (`up`)

- Fast happy path: if config requirements are already satisfied, `dir2mcp up` MUST not prompt.
- Interactive by default on TTY: if required config is missing and stdin/stdout is a TTY, `up` SHOULD run a guided setup flow.
- Scriptable always: every prompted value MUST have an env/flag/config equivalent.
- Non-interactive mode: with `--non-interactive` (or non-TTY), missing required values MUST return exit code `2` with explicit remediation instructions.
- Server-first semantics: server starts immediately when preflight requirements are satisfied. If required credentials for enabled ingestion/retrieval paths are missing, setup/validation runs before bind.
- Prompt masking: secret inputs (API keys/tokens) MUST be masked and never echoed.
- Preflight checks (minimum):
  - embeddings enabled -> requires Mistral API key (or configured local/on-prem embed connector)
  - OCR enabled for present/targeted PDFs/images -> requires OCR-capable provider credentials/connectors
  - STT enabled -> requires selected provider credentials/connectors
- Prompt parity examples:
  - Mistral API key -> `MISTRAL_API_KEY` or config-managed secret source
  - STT provider credentials -> provider-specific env vars or secret source
  - OCR/transcription enablement -> config keys under `ingest.*` and `stt.*`

---

## 3) CLI output contract

### 3.1 Human output (default)
On `dir2mcp up`, stdout MUST print:

1) Index/state location + mode  
2) MCP connection block (URL, headers, where token is stored)  
3) Progress lines during indexing

Example (illustrative):
```txt
Index: /repo/.dir2mcp  (meta.sqlite + vectors_text.hnsw + vectors_code.hnsw)
Mode: incremental  (server-first; indexing in background)

MCP endpoint:
  URL:    http://127.0.0.1:52143/mcp
  Auth:   Bearer (source=file, from --auth file:/run/secrets/dir2mcp.token)
  Headers:
    MCP-Protocol-Version: 2025-11-25
    Authorization: Bearer <token>
    MCP-Session-Id: (assigned after initialize response)

Progress: scanned=412 indexed=55 skipped=340 deleted=2 reps=88 chunks=1480 embedded=920 errors=1
```

Progress line fields (minimum):

* `scanned`, `indexed`, `skipped`, `deleted`
* `reps` (representations created/updated)
* `chunks` (chunks total known/created)
* `embedded` (chunks embedded successfully)
* `errors` (non-fatal per-document failures)

### 3.2 NDJSON output (`--json`)

Emit NDJSON (one JSON object per line), schema:

```json
{
  "ts": "2026-02-25T12:34:56.789Z",
  "level": "info|warn|error",
  "event": "index_loaded|server_started|connection|scan_progress|embed_progress|file_error|payment_required|payment_verified|payment_settled|payment_failed|fatal",
  "data": {}
}
```

Required events for `up`:

* `index_loaded`
* `server_started`
* `connection` (endpoint + headers + token reference)
* periodic `scan_progress` and `embed_progress`
* `file_error` for per-document failures (non-fatal)
* if x402 is enabled: `payment_required`, `payment_verified`, `payment_settled`, `payment_failed`

`connection.data` must include:

* `transport: "mcp_streamable_http"`
* `url`
* `headers` (include MCP-Protocol-Version, Authorization placeholder)
* `token_source` (`secret.token|env|file`)

If `--auth file:<path>` is used, `token_source` MUST be `file`, and the connection payload SHOULD include either `token_file` (preferred) or `token_source_details.path` to distinguish user-provided token files from auto-generated `.dir2mcp/secret.token`.

Example `connection.data` (file-auth mode):

```json
{
  "transport": "mcp_streamable_http",
  "url": "http://127.0.0.1:52143/mcp",
  "headers": {
    "MCP-Protocol-Version": "2025-11-25",
    "Authorization": "Bearer <token>"
  },
  "token_source": "file",
  "token_file": "/run/secrets/dir2mcp.token"
}
```

### 3.3 Non-interactive missing-config error contract

When required config is missing and prompts are disabled (`--non-interactive` or non-TTY), CLI output MUST be actionable and MUST NOT print secret values.

Example:

```txt
ERROR: CONFIG_INVALID: Missing MISTRAL_API_KEY
Set env: MISTRAL_API_KEY=...
Or run: dir2mcp config init
```

### 3.4 Hosted demo smoke probe (operational runbook)

For a hosted endpoint readiness check, use `./scripts/smoke_hosted_demo.sh`.
Set `DIR2MCP_DEMO_TOKEN` whenever the hosted MCP endpoint enforces auth
(bearer token required). It is optional only for deployments with no auth; if
you omit it against an auth-enabled endpoint the script can fail early (for
example with HTTP `401`) before the MCP pass conditions below are evaluated.

```bash
DIR2MCP_DEMO_URL="https://your-host.example/mcp" \
DIR2MCP_DEMO_TOKEN="<optional-bearer-token>" \
./scripts/smoke_hosted_demo.sh
```

Expected pass conditions:

* `initialize` returns HTTP `200` and includes `MCP-Session-Id`.
* `tools/list` returns HTTP `200` and includes tool metadata.
* `tools/call` against `dir2mcp.list_files` returns either:
  * HTTP `200` with JSON-RPC body, or
  * HTTP `402` with `PAYMENT-REQUIRED` when x402 route gating is enabled.

---

## 4) On-disk outputs (state)

All state lives under `<state-dir>` (default `<root>/.dir2mcp/`).

### 4.1 Layout

```
.dir2mcp/
  .dir2mcp.yaml.snapshot        # effective config snapshot (resolved values)
  connection.json               # connect info (no session id; assigned at initialize)
  secret.token                  # bearer token (0600)
  meta.sqlite                   # metadata store (documents/reps/chunks/spans)
  vectors_text.hnsw             # ANN index for text-like chunks
  vectors_code.hnsw             # ANN index for code chunks
  corpus.json                   # profile + progress summary
  ingest.log                    # optional
  cache/
    ocr/                        # cached OCR outputs (optional)
    transcribe/                 # cached transcripts (optional)
    annotations/                # cached annotation JSON (optional)
  payments/
    pricing.snapshot.json       # effective price policy (optional)
    settlement.log              # payment verification/settlement outcomes (optional)
  locks/
    index.lock
```

### 4.2 `secret.token`

* Contains a single bearer token line.
* Permissions MUST be restrictive (0600 on Unix-like systems).

### 4.3 `connection.json`

Written on `up`:

```json
{
  "transport": "mcp_streamable_http",
  "url": "http://127.0.0.1:52143/mcp",
  "headers": {
    "MCP-Protocol-Version": "2025-11-25",
    "Authorization": "Bearer <token-from-secret.token>"
  },
  "session": {
    "uses_mcp_session_id": true,
    "header_name": "MCP-Session-Id",
    "assigned_on_initialize": true
  }
}
```

### 4.4 `corpus.json`

A lightweight summary:

```json
{
  "root": "/abs/root",
  "profile": {
    "doc_counts": { "code": 120, "md": 35, "pdf": 9, "audio": 3, "image": 14, "other": 7 },
    "code_ratio": 0.62
  },
  "models": {
    "embed_text": "mistral-embed",
    "embed_code": "codestral-embed",
    "ocr": "mistral-ocr-latest",
    "stt_provider": "mistral",
    "stt_model": "voxtral-mini-latest",
    "chat": "mistral-small-2506"
  },
  "indexing": {
    "job_id": "job_...",
    "running": true,
    "scanned": 412,
    "indexed": 55,
    "skipped": 340,
    "deleted": 2,
    "representations": 88,
    "chunks_total": 1480,
    "embedded_ok": 920,
    "errors": 1
  }
}
```

When indexing stats are unavailable (e.g., the ListFiles-only fallback path where no live
`IndexingState` is present), the fields `representations`, `chunks_total`, and `embedded_ok`
are set to `-1` to signal "not derivable". A value of `-1` is **not** an error; consumers
MUST treat it as "data unavailable" and MUST NOT treat it as a counter value.

Example snapshot emitted via the ListFiles-only fallback path:

```json
{
  "root": "/abs/root",
  "profile": {
    "doc_counts": { "code": 120, "md": 35 },
    "code_ratio": 0.77
  },
  "models": {
    "embed_text": "mistral-embed",
    "embed_code": "codestral-embed",
    "ocr": "mistral-ocr-latest",
    "stt_provider": "mistral",
    "stt_model": "voxtral-mini-latest",
    "chat": "mistral-small-2506"
  },
  "indexing": {
    "job_id": "",
    "running": false,
    "mode": "incremental",
    "scanned": 155,
    "indexed": 120,
    "skipped": 35,
    "deleted": 0,
    "representations": -1,
    "chunks_total": -1,
    "embedded_ok": -1,
    "errors": 0
  }
}
```

---

## 5) SQLite metadata schema (minimum semantics)

The exact SQL types may vary; semantics must match.

### 5.1 `documents`

* `doc_id` (PK)
* `rel_path` (unique, normalized `/`)
* `source_type` (`file|archive_member`)
* `doc_type` (`code|text|md|pdf|image|audio|data|html|archive|binary_ignored|...`)
* `size_bytes`
* `mtime_unix`
* `content_hash` (stable, e.g., blake3/sha256)
* `status` (`ok|skipped|error`)
* `error` (nullable)
* `deleted` (boolean; tombstone)

### 5.2 `representations`

* `rep_id` (PK)
* `doc_id` (FK)
* `rep_type` (`raw_text|ocr_markdown|transcript|annotation_text|annotation_json`)
* `rep_hash` (stable; changes when rep changes)
* `created_unix`
* `meta_json` (must include provider/model for OCR/transcription/annotations when applicable)
* `deleted` (boolean; tombstone)

**Transcript meta_json requirements**

* `provider`: `mistral|elevenlabs`
* `model`: string
* `timestamps`: boolean
* `language`: optional
* `duration_ms`: optional

### 5.3 `chunks`

* `chunk_id` (PK; integer; used as ANN label)
* `rep_id` (FK)
* `ordinal`
* `text` (or compressed blob)
* `text_hash`
* `tokens_est` (approx)
* `index_kind` (`text|code`)  # routes to vectors_text or vectors_code
* `embedding_status` (`ok|pending|error`)
* `embedding_error` (nullable)
* `deleted` (boolean; tombstone)

### 5.4 `spans` (provenance for citations)

* `chunk_id` (FK)
* `span_kind` (`lines|page|time`)
* `start` (integer)  # start_line / page / start_ms
* `end` (integer)    # end_line / page / end_ms
* `extra_json` (nullable)  # speaker, confidence, section title, etc.

### 5.5 `settings`

* `key`, `value` for:

  * `protocol_version` = `2025-11-25`
  * `corpus_id`
  * `index_format_version`
  * `embed_text_model`, `embed_text_dim`
  * `embed_code_model`, `embed_code_dim`
  * `ocr_model`
  * `stt_provider`, `stt_model`
  * `chat_model`

---

## 6) Embedded ANN indices

### 6.1 Indices

* `vectors_text.hnsw`: embeddings for `index_kind=text` chunks (raw text, OCR markdown, transcripts, annotation_text).
* `vectors_code.hnsw`: embeddings for `index_kind=code` chunks (source code and code-like configs).

Dimensions may differ between indices; each index must be internally consistent.

### 6.2 Label mapping

* ANN label MUST equal `chunk_id` (integer), so a query result maps directly to chunk metadata.

### 6.3 Deletions (append-only index approach)

Indices are treated as append-only:

* Deleting documents/representations/chunks sets `deleted=1` in SQLite.
* Retrieval uses oversampling:

  * ask ANN for `k * oversample_factor` results
  * filter out `deleted=1`
  * return first `k` remaining
* Default `oversample_factor`: 5 (configurable).

---

## 7) Ingestion pipeline

### 7.1 Discovery

* Recursive walk from root.
* Default ignore list includes: `.git/`, `node_modules/`, `dist/`, `build/`, `.venv/`, `.dir2mcp/`.
* Optional `.gitignore` support.
* Symlink policy:

  * default: do not follow symlinks
  * if enabled: follow only if target resolves under root

### 7.2 Safety exclusions (default)

* Exclude obvious secrets/credentials patterns (regexes applied to file **contents**):

  * AWS Access Key ID: `AKIA[0-9A-Z]{16}`
  * AWS/Secret assignment heuristic: `(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`
  * JWTs: `(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}` (context-anchored)
  * Generic bearer token: `(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`
  * Common API key formats (e.g. `sk_[a-z0-9]{32}`, `api_[A-Za-z0-9]{32}`)

  These patterns are the **defaults**; they live in configuration under `security.secret_patterns` and can be extended or overridden by users.

  Expected false positives and tuning notes (map each note to its default rule):

  * **AWS Access Key ID** (`AKIA[0-9A-Z]{16}`): may match synthetic examples in docs/tests or random uppercase identifiers of the same shape.
  * **AWS/Secret assignment heuristic** (`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`): reduces prose false positives (for example “AWS Secrets Manager”) by requiring assignment-like context.
  * **JWTs** (`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`): reduced false positives via auth/key context and minimum segment lengths; can still match synthetic token-like test strings with those contexts.
  * **Generic bearer token** (`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`): can match innocuous config values named `token` (feature tokens, cache tokens) that are not credentials.
  * **Common API key formats** (`sk_[a-z0-9]{32}`, `api_[A-Za-z0-9]{32}`): can match placeholders, test fixtures, or generated IDs that happen to share the prefix/length.

  Refinement guidance via `security.secret_patterns`:

  * Tighten permissive rules (for example JWT/bearer) with context anchors such as preceding auth headers, key names, or delimiters.
  * Replace broad alternations with environment-specific patterns (known provider prefixes, expected lengths/alphabets).
  * Exclude known-safe paths with `security.path_excludes` (for example fixtures, snapshots, generated test vectors) instead of weakening global rules.
  * Keep broad defaults as a baseline, then add narrower allowlist/exception handling in path rules where operationally safe.

  Example tuning directions:

  * JWT: anchor to `Authorization: Bearer` or token key names and enforce minimum segment lengths.
  * Bearer token: constrain key names (`access_token`, `bearer_token`) and reduce accidental matches in generic `token=` fields.
  * AWS secret heuristic: keep assignment/credential context anchors and avoid broad prose matching.

  Testing approach for pattern updates:

  * Build a small positive/negative corpus per rule (must-hit secret samples and must-not-hit benign samples).
  * Run scanner tests in CI on both corpora and assert precision/recall thresholds appropriate for your risk posture.
  * Add regression fixtures for every incident-driven rule change (new false positive or false negative).
  * Review CI diffs of matched files/lines before merging pattern changes; iterate by tightening context anchors or path excludes.

* Exclude large binaries by default:

  * configurable max file size per `doc_type`.

* Path-based exclusions use optional `.gitignore`-style syntax. Users may provide
  additional ignore files or patterns via `security.path_excludes` in config
  (a list of glob patterns); the default set includes the same patterns used for
  ingestion (`.git/`, `node_modules/`, `.dir2mcp/`, etc.) plus any sensitive
  filenames detected.

### 7.3 Type classification

Use extension + MIME sniff + binary heuristics to classify:

* `code`: go/rs/py/js/ts/java/c/cpp/…
* `md/text/data/html`
* `pdf`, `image`, `audio`
* `archive` (zip/tar/tar.gz) optionally deep extracts members
* `binary_ignored`

### 7.4 Representation generation rules

#### A) Code/text/md/data/html

* Generate `raw_text` (normalized UTF-8, `\n` line endings).
* Route to index kind:

  * code → `index_kind=code`
  * others → `index_kind=text`

#### B) PDF/image

* Default: generate `ocr_markdown` via **Mistral OCR**.
* OCR is page-aware:

  * store page numbers as spans
  * chunk per page first
* Route to `index_kind=text`.
* Cache OCR output if enabled.

#### C) Audio (STT provider is configurable)

* Generate `transcript` via STT provider:

  * default: **Mistral STT**
  * optional: **ElevenLabs STT**
* If timestamps available:

  * segment into time windows (e.g., 30s with 5s overlap)
  * store spans as `time` (start_ms/end_ms)
* If timestamps not available:

  * fall back to text-size chunking
  * omit time spans
* Cache transcript if enabled.

#### D) Structured extraction (annotations)

* Default: on-demand only, via MCP tool.
* Store `annotation_json` representation.
* Optionally derive and embed `annotation_text`:

  * flattened `key: value` lines
  * route to `index_kind=text`

### 7.5 Chunking defaults

* Global character-based chunking:

  * `max_chars`, `overlap_chars`, `min_chars`
* Code:

  * line-window chunking (max_lines, overlap_lines)
  * store `lines` spans
* OCR:

  * per page, then within page by size constraints
  * store `page` spans
* Transcript:

  * segment by time if available
  * store `time` spans

### 7.6 Incremental indexing

* Document-level:

  * compute `content_hash`; if unchanged and not deleted → skip rep generation
* Representation-level:

  * compute `rep_hash`; if unchanged → skip chunk rebuild
* Chunk-level:

  * compute `text_hash`; if unchanged → skip embedding

### 7.7 Per-document error handling

Non-fatal per-doc errors:

* mark `documents.status=error`, record error
* continue indexing

Fatal errors:

* root inaccessible
* cannot write state (disk full, permissions)
* irrecoverable state corruption

---

## 8) Model/provider utilization requirements

### 8.1 Mistral (required)

dir2mcp MUST support direct HTTP calls from Go for:

* Embeddings:

  * `mistral-embed` (text index)
  * `codestral-embed` (code index)
* OCR:

  * `mistral-ocr-latest` (default) for PDFs/images
* Chat completions:

  * for RAG answering (default enabled)

### 8.2 STT providers

* Default STT provider: **Mistral** (offline transcription model).
* Optional STT provider: **ElevenLabs** (Scribe).
* Outputs MUST be normalized to the same `transcript` representation format.

### 8.3 Note on TTS

* TTS is optional and not required for core retrieval/inspection functionality.
* If enabled, it must remain additive and must not break non-TTS workflows.

---

## 9) Retrieval and answer generation

### 9.1 Search routing

At query time:

* `index=auto`:

  * default to `text`
  * choose `code` if query is code-oriented (heuristic) or filters target code
* `index=both`:

  * query both indices and fuse results
  * normalization: per-index score normalization then merge

### 9.2 Result structure and provenance

Each hit includes:

* `chunk_id`, `rel_path`, `rep_type`, `score`, `snippet`
* `span` with one of:

  * `lines` (start_line/end_line)
  * `page` (page)
  * `time` (start_ms/end_ms)

### 9.3 Citation formatting (human-readable)

Within answers, citations must be rendered as:

* code/text: `[path:L<start>-L<end>]`
* pdf OCR: `[path#p=<page>]`
* transcript: `[path@t=<start>-<end>]` where `<start>/<end>` are `mm:ss` or `ms`

### 9.4 RAG generation

If enabled:

* build a prompt with:

  * system prompt
  * question
  * retrieved contexts + citations
* return answer text + citations list + underlying hits (structured output)

If disabled or `mode=search_only`:

* return hits only.

---

## 10) MCP server: Streamable HTTP (2025-11-25)

### 10.1 Endpoint

* Default MCP path: `/mcp`
* POST accepts JSON-RPC messages (single object; batch arrays may be accepted optionally).

### 10.2 Required headers

Clients MUST send:

* `MCP-Protocol-Version: 2025-11-25` (after initialization)
* `Authorization: Bearer <token>` (unless auth disabled)
* `Accept: application/json, text/event-stream` (recommended)

Server returns:

* `MCP-Session-Id: <id>` on initialize response.

### 10.3 Sessions

* On initialize success, server assigns a session id and returns it in `MCP-Session-Id`.
* Client must include `MCP-Session-Id` on subsequent requests.
* Sessions are stateful resources with a defined lifecycle:

  * **Inactivity timeout:** a session SHOULD expire if the server has not seen any requests using that `MCP-Session-Id` for a configurable period. The reference implementation defaults to 24 hours of inactivity (matching the previous hardcoded `sessionTTL`), though some deployments may prefer shorter windows such as 30 minutes. Servers SHOULD expose a configuration parameter (e.g. `session_inactivity_timeout` as a YAML duration) so operators can adjust the value.
  * **Absolute lifetime (optional):** servers MAY enforce a maximum absolute duration (e.g. 24 hours) after which the session expires regardless of activity. In the reference implementation this is governed by `session_max_lifetime` (YAML duration); a zero value disables the limit.
  * **Cleanup/eviction:** expired sessions MUST be evicted or garbage‑collected from the server’s in‑memory or persisted session store. Cleanup can run lazily on access or via a periodic background task; the key requirement is that an expired `MCP-Session-Id` is treated as unknown.
  * **Logging & visibility:** servers SHOULD log session expiration events, including the reason (inactivity vs. lifetime) and the session id. Responses may include a diagnostic header such as `X-MCP-Session-Expired: inactivity|max-lifetime`.

* Unknown or expired session id:

  * server returns HTTP 404. This is the same status used for any non‑existent session; clients SHOULD treat both cases identically even if a diagnostic header is present.
  * client MUST re‑initialize by issuing a fresh `initialize` request. The previous id is discarded and a new `MCP-Session-Id` will be returned. Clients SHOULD treat a 404 as indicating that they should restart the flow rather than retrying.

* **Production guidance:**

  1. Choose default timeout values appropriate for your workload and security requirements. Public‑facing servers often use shorter inactivity timeouts to conserve resources.
  2. Expose configuration knobs for both inactivity and absolute lifetime. Document defaults in your service README.
  3. Surface expiration reasons in logs and, optionally, response headers to assist operators and clients.
  4. Implement robust cleanup to avoid unbounded session growth; periodic eviction or TTL caches are recommended.

### 10.4 Notifications

If a POST is a JSON-RPC notification (no id), and accepted:

* server returns HTTP `202 Accepted` and no body.

### 10.5 Origin checks (DNS rebinding mitigation)

If `Origin` header is present:

* must match allowlist
* otherwise return HTTP 403

### 10.6 Auth

* Bearer token required by default.
* Token storage: `.dir2mcp/secret.token`
* If `--auth file:<path>` is set, the token is loaded from that path, `connection.data.token_source` MUST be `file`, and `connection.data` SHOULD include `token_file` (or `token_source_details.path`).
* Tokens must not be embedded in URLs by default (avoid `?token=` in docs/outputs).

---

## 11) MCP lifecycle (wire-level)

All JSON-RPC messages are POSTed to the MCP endpoint.

### 11.1 `initialize` request (example)

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-11-25",
    "capabilities": { "tools": { } },
    "clientInfo": { "name": "example-client", "version": "0.0.1" }
  }
}
```

### 11.2 `initialize` response (example)

HTTP response headers include:

* `MCP-Session-Id: sess_...`

Body:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocolVersion": "2025-11-25",
    "capabilities": {
      "tools": { "listChanged": false }
    },
    "serverInfo": {
      "name": "dir2mcp",
      "title": "dir2mcp: Directory RAG MCP Server",
      "version": "0.4.0"
    },
    "instructions": "Use tools/list then tools/call. Results include citations."
  }
}
```

### 11.3 `notifications/initialized` (example)

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/initialized",
  "params": {}
}
```

Server returns: HTTP 202.

---

## 12) MCP tools: list and call

### 12.1 Tool naming

All tools are prefixed with `dir2mcp.`

### 12.2 Tool discovery: `tools/list`

Request:

```json
{ "jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {} }
```

Response contains an array of tools; each tool MUST include:

* `name`
* `description`
* `inputSchema` (valid JSON Schema object)
* (recommended) `outputSchema` (valid JSON Schema object)

### 12.3 Tool invocation: `tools/call`

Request:

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "tools/call",
  "params": { "name": "dir2mcp.search", "arguments": { "query": "..." } }
}
```

### 12.4 Tool result contract (MCP-native)

Tool call responses MUST return:

* `result.content[]` (at least one item)
* `result.structuredContent` when supported by negotiated version
* `result.isError` true for tool execution failures (not JSON-RPC error)

Example success:

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "content": [{ "type": "text", "text": "..." }],
    "structuredContent": { "...": "..." }
  }
}
```

Example tool execution error:

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "isError": true,
    "content": [{ "type": "text", "text": "ERROR: FILE_NOT_FOUND: audio/meeting.wav" }],
    "structuredContent": {
      "error": { "code": "FILE_NOT_FOUND", "message": "audio/meeting.wav not found", "retryable": false }
    }
  }
}
```

---

## 13) Tool set (core + recommended + optional)

### 13.1 Core tool set

* `dir2mcp.search`
* `dir2mcp.ask`
* `dir2mcp.open_file`
* `dir2mcp.list_files`
* `dir2mcp.stats`

### 13.2 Recommended extended tools

* `dir2mcp.transcribe` (audio → transcript, uses configured provider)
* `dir2mcp.annotate` (document → structured JSON + flattened text)
* `dir2mcp.transcribe_and_ask` (audio → transcript → ask)

### 13.3 Optional extension

* `dir2mcp.ask_audio` (answer → audio via ElevenLabs TTS)

---

## 14) Error taxonomy (canonical codes)

### 14.1 Auth/transport

* `UNAUTHORIZED` (missing/invalid token)
* `FORBIDDEN_ORIGIN` (Origin not allowed)
* `SESSION_NOT_FOUND` (invalid MCP-Session-Id)
* `BIND_FAILED` (cannot bind host/port)
* `TLS_CONFIG_INVALID`

### 14.2 Input validation

* `MISSING_FIELD`
* `INVALID_FIELD`
* `INVALID_RANGE`
* `FORBIDDEN` (path/content blocked by policy)
* `PATH_OUTSIDE_ROOT`
* `FILE_NOT_FOUND`
* `DOC_TYPE_UNSUPPORTED`

### 14.3 Index/state

* `CONFIG_INVALID`
* `STORE_CORRUPT`
* `INDEX_VERSION_MISMATCH`
* `INDEX_NOT_READY` (should be rare; prefer partial results)

### 14.4 Ingestion/extraction

* `EXTRACT_FAILED`
* `OCR_FAILED`
* `TRANSCRIBE_FAILED`
* `ANNOTATE_FAILED`
* `FILE_TOO_LARGE`
* `BINARY_SKIPPED`

### 14.5 Provider/API

* `MISTRAL_AUTH`
* `MISTRAL_RATE_LIMIT`
* `MISTRAL_FAILED`
* `ELEVENLABS_AUTH`
* `ELEVENLABS_RATE_LIMIT`
* `ELEVENLABS_FAILED`

Each tool error returns:

* `code`, `message`, `retryable` boolean.

---

## 15) Tool specifications (full schemas)

All schemas are JSON Schema (draft-agnostic, compatible with common validators).

### 15.1 Shared types

#### 15.1.1 `Span`

```json
{
  "type": "object",
  "oneOf": [
    {
      "additionalProperties": false,
      "properties": { "kind": { "const": "lines" }, "start_line": { "type": "integer" }, "end_line": { "type": "integer" } },
      "required": ["kind", "start_line", "end_line"]
    },
    {
      "additionalProperties": false,
      "properties": { "kind": { "const": "page" }, "page": { "type": "integer" } },
      "required": ["kind", "page"]
    },
    {
      "additionalProperties": false,
      "properties": { "kind": { "const": "time" }, "start_ms": { "type": "integer" }, "end_ms": { "type": "integer" } },
      "required": ["kind", "start_ms", "end_ms"]
    }
  ]
}
```

#### 15.1.2 `Hit`

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "chunk_id": { "type": "integer" },
    "rel_path": { "type": "string" },
    "doc_type": { "type": "string" },
    "rep_type": { "type": "string" },
    "score": { "type": "number" },
    "snippet": { "type": "string" },
    "span": { "$ref": "#/definitions/Span" }
  },
  "required": ["chunk_id", "rel_path", "score", "snippet", "span"]
}
```

> Note: In `tools/list`, you will inline these definitions or include them in each tool’s `outputSchema` as `definitions`.

---

### 15.2 `dir2mcp.search`

**Description:** semantic retrieval across indexed content.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "minLength": 1 },
    "k": { "type": "integer", "minimum": 1, "maximum": 50, "default": 10 },
    "index": { "type": "string", "enum": ["auto", "text", "code", "both"], "default": "auto" },
    "path_prefix": { "type": "string" },
    "file_glob": { "type": "string" },
    "doc_types": { "type": "array", "items": { "type": "string" } }
  },
  "required": ["query"]
}
```

**Output schema (structuredContent):**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string" },
    "k": { "type": "integer" },
    "index_used": { "type": "string", "enum": ["text", "code", "both"] },
    "hits": {
      "type": "array",
      "items": { "$ref": "#/definitions/Hit" }
    },
    "indexing_complete": { "type": "boolean" }
  },
  "required": ["query", "hits", "indexing_complete"]
}
```

**content[] requirements:**

* At least one `text` item summarizing results (top hits + citations).

---

### 15.3 `dir2mcp.ask`

**Description:** RAG answer with citations; can run search-only.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": { "type": "string", "minLength": 1 },
    "k": { "type": "integer", "minimum": 1, "maximum": 50, "default": 10 },
    "mode": { "type": "string", "enum": ["answer", "search_only"], "default": "answer" },
    "index": { "type": "string", "enum": ["auto", "text", "code", "both"], "default": "auto" },
    "path_prefix": { "type": "string" },
    "file_glob": { "type": "string" },
    "doc_types": { "type": "array", "items": { "type": "string" } }
  },
  "required": ["question"]
}
```

**Output schema (structuredContent):**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": { "type": "string" },
    "answer": { "type": "string" },
    "citations": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "chunk_id": { "type": "integer" },
          "rel_path": { "type": "string" },
          "span": { "$ref": "#/definitions/Span" }
        },
        "required": ["chunk_id", "rel_path", "span"]
      }
    },
    "hits": { "type": "array", "items": { "$ref": "#/definitions/Hit" } },
    "indexing_complete": { "type": "boolean" }
  },
  "required": ["question", "citations", "hits", "indexing_complete"]
}
```

**content[] requirements:**

* `text` item containing the final answer (if mode=answer and generation enabled) with inline citations.

---

### 15.4 `dir2mcp.open_file`

**Description:** open an exact source slice for verification (lines/page/time).

*Implementation note:* before reading or returning any data, the server MUST run the
requested `rel_path` and any extracted content through the configured exclusion
engine (pattern matcher + path excludes).  If a match occurs the tool **must not**
return the secret content; it should either return an error (e.g. `FORBIDDEN`)
or an empty/plain-text placeholder.  This ensures tool-level bypass of ingestion
filters is impossible.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string", "minLength": 1 },
    "start_line": { "type": "integer", "minimum": 1 },
    "end_line": { "type": "integer", "minimum": 1 },
    "page": { "type": "integer", "minimum": 1 },
    "start_ms": { "type": "integer", "minimum": 0 },
    "end_ms": { "type": "integer", "minimum": 0 },
    "max_chars": { "type": "integer", "minimum": 200, "maximum": 50000, "default": 20000 }
  },
  "required": ["rel_path"]
}
```

**Selection rules:**

* If `page` provided → return OCR page text (if available; else error `DOC_TYPE_UNSUPPORTED`).
* Else if `start_ms/end_ms` provided → return transcript excerpt (if available).
* Else if `start_line/end_line` provided → return file lines.
* Else default:

  * return first `max_chars` (or first N lines) for text/code,
  * return page 1 for PDF OCR if available,
  * return transcript beginning for audio.

**Output schema (structuredContent):**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string" },
    "doc_type": { "type": "string" },
    "span": { "$ref": "#/definitions/Span" },
    "content": { "type": "string" },
    "truncated": { "type": "boolean" }
  },
  "required": ["rel_path", "doc_type", "content", "truncated"]
}
```

---

### 15.5 `dir2mcp.list_files`

**Description:** list files under root for navigation and filter selection.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path_prefix": { "type": "string" },
    "glob": { "type": "string" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 5000, "default": 200 },
    "offset": { "type": "integer", "minimum": 0, "default": 0 }
  }
}
```

**Output schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "limit": { "type": "integer" },
    "offset": { "type": "integer" },
    "total": { "type": "integer" },
    "files": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "rel_path": { "type": "string" },
          "doc_type": { "type": "string" },
          "size_bytes": { "type": "integer" },
          "mtime_unix": { "type": "integer" },
          "status": { "type": "string", "enum": ["ok", "skipped", "error"] },
          "deleted": { "type": "boolean" }
        },
        "required": ["rel_path", "doc_type", "size_bytes", "mtime_unix", "status", "deleted"]
      }
    }
  },
  "required": ["limit", "offset", "total", "files"]
}
```

---

### 15.6 `dir2mcp.stats`

**Description:** status/progress/health for indexing and models.

**Input schema:**

```json
{ "type": "object", "additionalProperties": false }
```

**Output schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "root": { "type": "string" },
    "state_dir": { "type": "string" },
    "protocol_version": { "type": "string" },
    "doc_counts": { "type": "object", "additionalProperties": { "type": "integer" } },
    "total_docs": { "type": "integer" },
    "doc_counts_available": { "type": "boolean" },
    "indexing": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "job_id": { "type": "string" },
        "running": { "type": "boolean" },
        "mode": { "type": "string", "enum": ["incremental", "full"] },
        "scanned": { "type": "integer" },
        "indexed": { "type": "integer" },
        "skipped": { "type": "integer" },
        "deleted": { "type": "integer" },
        "representations": {
          "type": "integer",
          "minimum": -1,
          "description": "Number of representations created/updated. -1 means not derivable (ListFiles-only fallback path); treat as unavailable, not as an error."
        },
        "chunks_total": {
          "type": "integer",
          "minimum": -1,
          "description": "Total chunks known/created. -1 means not derivable (ListFiles-only fallback path); treat as unavailable, not as an error."
        },
        "embedded_ok": {
          "type": "integer",
          "minimum": -1,
          "description": "Chunks embedded successfully. -1 means not derivable (ListFiles-only fallback path); treat as unavailable, not as an error."
        },
        "errors": { "type": "integer" }
      },
      "required": ["job_id", "running", "mode", "scanned", "indexed", "skipped", "deleted", "representations", "chunks_total", "embedded_ok", "errors"]
    },
    "models": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "embed_text": { "type": "string" },
        "embed_code": { "type": "string" },
        "ocr": { "type": "string" },
        "stt_provider": { "type": "string", "enum": ["mistral", "elevenlabs"] },
        "stt_model": { "type": "string" },
        "chat": { "type": "string" }
      },
      "required": ["embed_text", "embed_code", "ocr", "stt_provider", "stt_model", "chat"]
    }
  },
  "required": ["root", "state_dir", "protocol_version", "doc_counts", "total_docs", "doc_counts_available", "indexing", "models"]
}
```

---

### 15.7 `dir2mcp.transcribe` (recommended)

**Description:** force transcription for an audio file, persist transcript representation, and (optionally) return segments.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string", "minLength": 1 },
    "language": { "type": "string" },
    "timestamps": { "type": "boolean", "default": true },
    "retranscribe": { "type": "boolean", "default": false }
  },
  "required": ["rel_path"]
}
```

**Output schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string" },
    "provider": { "type": "string", "enum": ["mistral", "elevenlabs"] },
    "model": { "type": "string" },
    "indexed": { "type": "boolean" },
    "segments": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "start_ms": { "type": "integer" },
          "end_ms": { "type": "integer" },
          "text": { "type": "string" }
        },
        "required": ["start_ms", "end_ms", "text"]
      }
    }
  },
  "required": ["rel_path", "provider", "model", "indexed"]
}
```

---

### 15.8 `dir2mcp.annotate` (recommended)

**Description:** run structured extraction on a document with provided JSON schema; store JSON; optionally index flattened text.

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string", "minLength": 1 },
    "schema_json": { "type": "object" },
    "index_flattened_text": { "type": "boolean", "default": true }
  },
  "required": ["rel_path", "schema_json"]
}
```

**Output schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string" },
    "stored": { "type": "boolean" },
    "flattened_indexed": { "type": "boolean" },
    "annotation_json": { "type": "object" },
    "annotation_text_preview": { "type": "string" }
  },
  "required": ["rel_path", "stored", "flattened_indexed", "annotation_json"]
}
```

---

### 15.9 `dir2mcp.transcribe_and_ask` (recommended)

**Description:** ensure transcript exists (transcribe if missing/stale), then answer a question using transcript (and optionally whole corpus if configured).

**Input schema:**

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "rel_path": { "type": "string", "minLength": 1 },
    "question": { "type": "string", "minLength": 1 },
    "k": { "type": "integer", "minimum": 1, "maximum": 50, "default": 10 }
  },
  "required": ["rel_path", "question"]
}
```

**Output schema:** same as `dir2mcp.ask` output schema, plus:

* `transcript_provider`, `transcript_model`, and `transcribed` boolean.

---

### 15.10 `dir2mcp.ask_audio` (optional extension)

**Description:** same as `ask` but includes audio output (TTS). Optional and additive.

Input schema:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": { "type": "string", "minLength": 1 },
    "voice_id": { "type": "string" },
    "format": { "type": "string", "enum": ["mp3", "wav"], "default": "mp3" }
  },
  "required": ["question"]
}
```

Tool result `content[]` must include:

* `text` item for answer
* `audio` item with base64 payload and mimeType

---

## 16) Configuration (single file)

### 16.1 Precedence

1. CLI flags
2. env vars
3. `.dir2mcp.yaml`
4. defaults

### 16.1.1 Secret source precedence

For credential material (API keys/tokens), the runtime resolves sources in this order:

1. Explicit env var (for example `MISTRAL_API_KEY`, `ELEVENLABS_API_KEY`, `DIR2MCP_AUTH_TOKEN`)
2. OS keychain entry (when configured and supported)
3. Configured secret file reference (for example `security.auth.token_file`)
4. Interactive session-only value (not persisted; TTY flow only)

The config snapshot (`.dir2mcp.yaml.snapshot`) MUST record secret source metadata (env/keychain/file/session) and MUST NOT contain plaintext secrets.

### 16.2 Minimal config template (dual STT, 2025-11-25)

```yaml
version: 1

mistral:
  api_key: ${MISTRAL_API_KEY}
  chat_model: mistral-small-2506
  embed_text_model: mistral-embed
  embed_code_model: codestral-embed
  ocr_model: mistral-ocr-latest

rag:
  generate_answer: true
  k_default: 10
  system_prompt: |
    You are a retrieval-augmented assistant.
    Use citations and never invent sources.
  max_context_chars: 20000
  oversample_factor: 5

ingest:
  gitignore: true
  pdf:
    mode: ocr          # off|ocr|auto
  images:
    mode: ocr_auto     # off|ocr_auto|ocr_on
  audio:
    mode: auto         # off|auto|on
    cache: true
  archives:
    mode: deep         # off|shallow|deep
  follow_symlinks: false
  max_file_mb: 20

chunking:
  max_chars: 2500
  overlap_chars: 250
  min_chars: 200
  code:
    max_lines: 200
    overlap_lines: 30
  transcript:
    segment_ms: 30000
    overlap_ms: 5000

stt:
  provider: mistral        # mistral|elevenlabs
  mistral:
    api_key: ${MISTRAL_API_KEY}
    model: voxtral-mini-latest
    timestamps: true
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: scribe_v1
    timestamps: true

x402:
  # `mode` is the primary, authoritative field controlling whether
  # payment gating is active.  Allowed values are `off`, `on`, and
  # `required` (see x402 spec).  The configuration loader normalizes
  # the mode string and writes it back into the struct during
  # validation.  After validation callers should rely solely on
  # `mode`.
  #
  # `enabled` is retained only for historical compatibility and
  # as a convenience for simple boolean cases; it is not consulted by
  # the loader.  When both fields are present, `mode` wins and `enabled`
  # is effectively derived (`enabled` == mode != "off").  Operators are
  # encouraged to specify `mode` exclusively and may treat `enabled` as
  # deprecated.  Future releases may drop `enabled` entirely.
  enabled: false               # deprecated; use `mode` instead (see note above)
  mode: off                    # off|on|required
  facilitator_url: ""
  resource_base_url: ""
  route_policy:
    tools_call:
      enabled: false
      price: "0.001"
      network: "eip155:8453"
      scheme: "exact"
      asset: ""
      pay_to: ""
  bazaar:
    enabled: false
    metadata:
      description: ""

server:
  listen: "127.0.0.1:0"
  mcp_path: "/mcp"
  protocol_version: "2025-11-25"
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
  public: false

# session timeouts for MCP sessions
# default inactivity ~24h, adjust as needed
# session_max_lifetime zero disables absolute limit
session_inactivity_timeout: "24h"
session_max_lifetime: "0"

secrets:
  provider: auto         # auto|keychain|file|env|session
  keychain:
    service: "dir2mcp"
    account: "default"
  file:
    path: ".dir2mcp/secret.env"
    mode: "0600"

security:
  auth:
    mode: auto           # auto|none|file
    token_file: ""       # used when mode=file
    token_env: "DIR2MCP_AUTH_TOKEN"
  allowed_origins:
    - "http://localhost"
    - "http://127.0.0.1"
  path_excludes:
    - "**/.git/**"
    - "**/node_modules/**"
    - "**/.dir2mcp/**"
    - "**/.env"
    - "**/*.pem"
    - "**/*.key"
    - "**/id_rsa"
  secret_patterns:
    - 'AKIA[0-9A-Z]{16}'
    - '(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}'
    - '(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}'
    - '(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}'
    - 'sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}'
```

---

## 17) Security and safety requirements (minimum)

* Root isolation: reject any `rel_path` resolving outside root (`PATH_OUTSIDE_ROOT`).
* Symlink policy: default no-follow, or follow only if resolved under root.
* Archive safety: prevent zip-slip/path traversal within archives.
* Auth:
  * bearer token required by default
  * do **not** pass tokens on the command line; arguments may be exposed to other users or processes
  * use `--auth file:<path>` to point to a user-provided token file with restrictive permissions (the auto-generated `.dir2mcp/secret.token` is created with `0600`, but any secured path works)
  * alternatively, set `DIR2MCP_AUTH_TOKEN` for environment-based tokens
  * token file path and environment variable are equivalent sources; `--auth file:` tells dir2mcp where to read the token
  * config parity: `security.auth.token_file` specifies a file path and `security.auth.token_env` specifies an environment variable name; when either is set in `.dir2mcp.yaml` the behavior is equivalent (providing a token from the named source) but they refer to different source types.
* Secret handling:
  * secret input in interactive prompts MUST be masked
  * plaintext secrets MUST never be written to logs, terminal progress lines, NDJSON events, or config snapshots
  * preferred storage is OS keychain when available; file storage is fallback and MUST enforce `0600` permissions
  * if `secrets.provider=session`, secrets are process-memory only and are discarded at exit
* Origin checks:

  * if `Origin` header is present, enforce allowlist
* Sensitive file defaults:

  * default excludes include the secret regex patterns listed in §7.2, and
    these patterns are configurable via `security.secret_patterns`.
  * the exclusion engine is consulted on every file access, including
    `open_file`; tool handlers must reject or return an empty result for
    any path or content matching the configured patterns or path excludes.

---

## 18) Native x402 integration requirements (minimum)

* x402 mode is optional and must be switchable via config/flags (`off|on|required`).
* Payment enforcement MUST happen at the HTTP/MCP request boundary, not in retrieval/indexing internals.
* When a paid route is called without valid payment, server returns HTTP `402 Payment Required` with machine-readable payment requirements in `PAYMENT-REQUIRED`.
* Paid retry requests MUST be validated from `PAYMENT-SIGNATURE` (x402 v2 semantics).
* For paid requests, verification and settlement MUST be delegated to a facilitator (hosted or self-managed); dir2mcp remains non-custodial.
* Successful paid responses SHOULD include facilitator settlement metadata via `PAYMENT-RESPONSE` when available.
* x402 network identifiers MUST use CAIP-2 format (for example: `eip155:8453`, `eip155:84532`, `solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`).
* Recommended paid scope: gate `tools/call` (or selected tool names); keep lifecycle (`initialize`, `tools/list`) ungated.
* Payment failures MUST map to canonical tool/transport errors (`UNAUTHORIZED`, `MISTRAL_FAILED`, plus x402-specific payment failure metadata).
* If enabled, server should emit payment telemetry in NDJSON (`payment_required|payment_verified|payment_settled|payment_failed`).
* Bazaar/discovery metadata is optional and additive; lack of Bazaar metadata must not affect core MCP behavior.
* If Bazaar support is enabled, discovery metadata SHOULD be emitted via x402 extension metadata and resolved through facilitator discovery APIs (for example, `GET {facilitator_url}/discovery/resources`).

---

## 19) Non-goals (scope control)

* No external vector DB backends (no Qdrant).
* No in-place deletions in ANN index (use tombstones + oversampling).
* No marketplace inside dir2mcp.
* No requirement that audio output (TTS) be enabled for core retrieval/inspection workflows.
* No “agent that executes shell commands” (dir2mcp is retrieval/inspection only).

---

## 20) Implementation guidance (non-normative)

Suggested Go libraries for the interactive CLI experience:

* CLI parsing: `spf13/cobra`
* Prompt flow (wizard/select/masked input): `charmbracelet/huh`
* Output styling/layout: `charmbracelet/lipgloss`
* Optional progress spinner: `briandowns/spinner`
* TTY detection: `golang.org/x/term`
* OS keychain integration: `github.com/zalando/go-keyring`

These are recommendations, not protocol requirements.
