# Dual-machine deployment — dir2mcp on a GPU VPS with a remote corpus

This guide describes the lowest-friction way to run dir2mcp when your corpus and
your inference compute live in different places: run the dir2mcp **daemon on a
GPU VPS**, run your model servers (embed / OCR / STT) on **localhost** next to
it, and read the corpus from a **remote source** (an NFS mount or an S3 bucket).

This is an operator guide. For the normative contracts behind every key
referenced here, see the canonical spec in the `dirstral-spec` submodule:

- Self-hosted / OpenAI-compatible provider endpoints — `dirstral-spec/docs/SPEC.md` §8.5
- Remote corpus sources — `dirstral-spec/docs/SPEC.md` §7.8
- Vector index backends and identity — `dirstral-spec/docs/SPEC.md` §6
- Credential resolution (env → keychain → `.env.local`, never persisted) — `dirstral-spec/docs/SPEC.md` §16.1.1
- Config schema (`source:`, `index:`, `providers:`, `model:`) — `dirstral-spec/docs/SPEC.md` §16.2

The pointer stubs in this directory ([`SPEC.md`](SPEC.md), [`VISION.md`](VISION.md),
[`ECOSYSTEM.md`](ECOSYSTEM.md), [`x402-payment-adapter-spec.md`](x402-payment-adapter-spec.md))
link the same canonical documents.

---

## 1. Topology and rationale

```
                        GPU VPS
   ┌──────────────────────────────────────────────────┐
   │                                                    │
   │   dir2mcp daemon ──localhost──> embed server       │
   │        │          ──localhost──> OCR server        │
   │        │          ──localhost──> STT (whisper)     │
   │        │                                           │
   │        ├─ StateDir  (ALWAYS local: SQLite +        │
   │        │             vector index + caches)        │
   │        │                                           │
   │        └─ corpus read ───────────┐                 │
   │                                  │                 │
   └──────────────────────────────────┼────────────────┘
              ▲ (only the MCP          │ corpus bytes
              │  endpoint is exposed)  ▼
        MCP client                NFS mount  /  S3 bucket
       (Claude, etc.)             (remote corpus content)
```

Why this shape:

- **Daemon on the GPU VPS, inference on localhost.** Embedding, OCR, and STT are
  the GPU-bound work. Running the daemon on the same box means every model call
  is a `localhost` round-trip — no inference traffic crosses the public network,
  and the only exposed surface is the MCP endpoint itself.
- **Co-locate compute with the corpus *read*.** Indexing reads every byte of the
  corpus at least once, and multimodal ingest (audio/video transcription, PDF
  page rendering) re-reads media. Putting the daemon next to where the corpus is
  read avoids pulling the same bytes across the wire twice. This matters most for
  **video**, which is large and, for time-window extraction, currently
  materializes the whole object locally (see [§4 caveats](#4-caveats-and-current-limitations)).
- **StateDir stays local — always.** Regardless of where the corpus lives, the
  state directory (SQLite metadata, the embedded vector index, and caches) is
  kept on the VPS's local disk. dir2mcp never writes its index or state back to
  the remote source; only the corpus *content* is remote. (`dirstral-spec/docs/SPEC.md`
  §7.8, §1.2.) When the corpus is on S3, the per-object download cache lives
  under `StateDir/corpus-cache`.

---

## 2. Step-by-step

### 2.1 Stand up the inference servers (localhost)

Run your embed / OCR / STT servers on the VPS, each listening on a loopback
port. dir2mcp speaks the **OpenAI-compatible** wire protocol for embed and chat,
and the standard OpenAI `POST {base_url}/v1/audio/transcriptions` multipart
contract for self-hosted STT, so any server implementing those endpoints works
(vLLM, Ollama, LM Studio, a whisper OpenAI shim, etc.). A self-hosted server on
a trusted network may be **credential-less** — no API key required.

> OCR note: OCR is **not** part of the OpenAI-compatible surface (SPEC §8.5: OCR
> has no OpenAI analog). A self-hosted `kind: openai` profile is eligible for
> embed/chat/STT but **not** for OCR — binding `model.ocr.provider` to a
> `kind: openai` profile is rejected as `CONFIG_INVALID`. There are two
> self-hosted OCR routes:
>
> 1. **docling-serve** (recommended) — run a [docling-serve](https://github.com/docling-project/docling-serve)
>    container on the VPS and point `ingest.docling.serve_url` at it (e.g.
>    `http://localhost:5001`). This is a distinct extractor, not a provider
>    profile; see [Document extraction over HTTP](../README.md#docling-extraction-over-http-docling-serve).
> 2. **A self-hosted `kind: mistral` `/v1/ocr` endpoint** — declare a
>    `kind: mistral` profile whose `base_url` is the local OCR host and bind it
>    via `model.ocr.provider` (shown below). dir2mcp will POST the bespoke
>    `{base_url}/v1/ocr` request to that endpoint instead of the hosted Mistral
>    default.

### 2.2 Point provider `base_url`s at localhost

A self-hosted server is just an `openai`-kind provider profile whose `base_url`
points at the local endpoint — **no new provider `kind` is introduced**. Declare
the profiles under `providers:` and bind capabilities under `model:`:

```yaml
providers:
  # Self-hosted embed/chat server on the VPS (credential-less example: no api_key).
  vllm:
    kind: openai
    base_url: http://localhost:8000/v1
    embed_text_model: <your-embed-model>
    embed_code_model: <your-embed-model>
    chat_model: <your-chat-model>

  # Self-hosted STT. The built-in `whisper` profile is kind: whisper and
  # defaults its base_url from ${WHISPER_BASE_URL}; you can override it here.
  whisper:
    kind: whisper
    base_url: http://localhost:9000
    stt_model: <your-whisper-model>   # optional; servers often accept whisper-1

  # Self-hosted OCR via a kind: mistral /v1/ocr endpoint (optional — see the OCR
  # note above; docling-serve is the other route). base_url is the host ROOT:
  # the bespoke OCR client appends /v1/ocr itself (unlike kind: openai base_urls,
  # which already include /v1). A trusted-network endpoint may be credential-less.
  gpu-ocr:
    kind: mistral
    base_url: http://localhost:9100
    ocr_model: <your-ocr-model>

model:
  embed:
    provider: vllm
  chat:
    provider: vllm
  ocr:
    provider: gpu-ocr               # omit to use the hosted mistral-ocr default

# Self-hosted STT is reached via the legacy stt_provider selector.
stt_provider: whisper
```

Notes verified against the code:

- The built-in `local` profile (`kind: openai`, `base_url: http://localhost:11434/v1`)
  and `whisper` profile (`kind: whisper`, `base_url: ${WHISPER_BASE_URL}`) are
  both shipped. Both are **excluded from auto-selection precedence**, so they
  never silently win — reach them via an explicit `model.<cap>.provider` binding
  (or `stt_provider: whisper` for STT).
- Per-capability model-name overrides may also be set in the `model:` block
  (`model.embed.text_model` / `code_model` / `text_dim` / `code_dim`,
  `model.chat.model`, `model.ocr.model`).

### 2.3 Configure the corpus source (`source:`)

Pick the corpus backend with `source.kind`. The default `local` is unchanged;
`nfs` is treated identically to `local` (an NFS mount is just a local path);
`s3` reads from an object store.

**NFS** — mount the share on the VPS, point `RootDir` (the `--dir` root) at the
mount, and set:

```yaml
source:
  kind: nfs
```

**S3** (or an S3-compatible store such as MinIO / Cloudflare R2):

```yaml
source:
  kind: s3
  s3:
    bucket: my-corpus-bucket
    prefix: corpus/            # optional; scopes the corpus to a key prefix
    region: us-east-1          # optional; falls back to the AWS default chain
    endpoint: https://minio.local   # optional; for S3-compatible stores
```

AWS credentials are resolved **at runtime** through the standard precedence
(environment → keychain → `.env.local`) from `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and optionally `AWS_SESSION_TOKEN`. They are **never
persisted** to the config file or the effective-config snapshot. With
`source.kind: s3`, startup fails loudly (`CONFIG_INVALID`-style) if the bucket is
missing or the access key + secret are not resolvable.

`rel_path` (document identity) is stable across schemes: for `local`/`nfs` it is
the path under the root; for `s3` it is the object key minus the configured
prefix. A corpus can be relocated `local ⇄ nfs ⇄ s3` without forcing a reindex
on relocation alone. (`dirstral-spec/docs/SPEC.md` §7.8.)

### 2.4 Choose the vector backend (`index.backend`)

| `index.backend` | Tier | External infra | When |
|---|:--:|:--:|---|
| `memory` (default) | A | none | In-memory HNSW, pure-Go, snapshotted to StateDir. Zero-infra default. |
| `disk` | B | none | Pure-Go on-disk / memmapped single-node index; for corpora too large for RAM. |
| `qdrant` | C | required | External Qdrant collection. |
| `pgvector` | C | required | External PostgreSQL + pgvector. |

`memory` and `disk` keep all index state under the local StateDir and need no
external service. The Tier-C backends are **optional** and connect to an external
store:

```yaml
index:
  backend: qdrant
  qdrant:
    url: http://localhost:6334       # required for qdrant
    collection: my-corpus            # optional; per-kind collections derived (_text/_code)
    # api_key: ${QDRANT_API_KEY}     # runtime-only secret; only for secured/Cloud
```

```yaml
index:
  backend: pgvector
  pgvector:
    dsn: ${PGVECTOR_DSN}             # runtime-only secret; never persisted
    schema: public                   # optional
    table: vectors                   # optional; code axis appends _code
```

Tier-C connection secrets (`qdrant.api_key`, `pgvector.dsn`) follow the same
runtime-only credential rules and are never written to disk. A configured Tier-C
backend that is unreachable at preflight fails startup rather than silently
downgrading. (`dirstral-spec/docs/SPEC.md` §6.2–6.3.)

### 2.5 Start the daemon and expose only the MCP endpoint

Bring up the daemon on the VPS with the corpus root and a local state directory.
Expose only the MCP listen address to clients; keep the inference ports bound to
loopback. If you expose the endpoint publicly, remember that `--public` requires
auth unless `--force-insecure` is explicitly set.

---

## 3. How the pieces fit (data flow)

1. The daemon enumerates the corpus from `source.kind` (filesystem walk for
   `local`/`nfs`; flat object listing for `s3`).
2. For each document it computes/embeds chunks by calling the localhost embed
   server; OCR/STT run on their localhost servers for images/media.
3. Vectors land in the configured `index.backend` (local StateDir for
   `memory`/`disk`; an external store for `qdrant`/`pgvector`).
4. SQLite metadata, the embedded index, and caches stay under the local
   StateDir — never on the remote corpus source.

---

## 4. Caveats and current limitations

- **FUSE-over-S3 listing cost.** If you mount S3 as a filesystem (mountpoint-s3,
  goofys, etc.) instead of using `source.kind: s3`, directory listings over a
  large object tree can be slow and request-expensive, because a filesystem walk
  turns into many `LIST`/`HEAD` calls. Prefer the native `source.kind: s3`
  backend (a flat object listing) for large corpora; reserve FUSE for cases where
  you specifically need a real mountpoint.

- **Video time-window reads currently download the whole object.** Range reads
  over S3 avoid whole-object downloads for plain byte-slice reads, but the
  ffmpeg/archive paths use `Localize`, which materializes the **entire object** to
  the local download cache (`StateDir/corpus-cache`) before extracting a time
  window. For large video files on a remote source this means each windowed
  extraction pulls the full file. This is the strongest reason to **co-locate the
  daemon with the corpus read** and to keep StateDir on fast local disk.

- **Embed-identity reindex when switching the self-hosted embed model.** The
  corpus-lifetime **embed identity** (`provider | text_model | code_model |
  text_dim | code_dim | multimodal`) binds the index regardless of backend. If
  you change the self-hosted embed provider or model (or its dimension), the
  recorded identity no longer matches and dir2mcp refuses to mix vector spaces —
  it requires a full reindex. Plan an embed-model change as a reindex, not a
  hot-swap. (`dirstral-spec/docs/SPEC.md` §6.4, §8.1.4.)

---

## 5. Related docs

- [`SPEC.md`](SPEC.md) — pointer to the canonical, normative spec (this guide's
  references resolve there): §6 (vector backends), §7.8 (remote corpus), §8.5
  (self-hosted endpoints), §16.1.1 (credentials), §16.2 (config schema).
- [`VISION.md`](VISION.md), [`ECOSYSTEM.md`](ECOSYSTEM.md) — product and ecosystem context.
- [`x402-payment-adapter-spec.md`](x402-payment-adapter-spec.md) — request-gating
  adapter contract, if you gate the exposed MCP endpoint.
