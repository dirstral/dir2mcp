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
- Distributed embedding (coordinator + workers + broker) — `dirstral-spec/docs/SPEC.md` §8.7
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
  materializes the whole object locally (see [§5 caveats](#5-caveats-and-current-limitations)).
- **StateDir stays local — always.** Regardless of where the corpus lives, the
  state directory (SQLite metadata, the embedded vector index, and caches) is
  kept on the VPS's local disk. dir2mcp never writes its index or state back to
  the remote source; only the corpus *content* is remote. (`dirstral-spec/docs/SPEC.md`
  §7.8, §1.2.) When the corpus is on S3, the per-object download cache lives
  under `StateDir/corpus-cache`.

The single-daemon shape above is the default and the recommended starting point.
If embedding throughput becomes the bottleneck, you can optionally fan the embed
work out across several worker processes on the VPS while keeping one daemon as the
coordinator — see [§4 Optional: distributed embedding](#4-optional-distributed-embedding-coordinator--workers).

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

**No local corpus directory is required for `s3`.** The bucket + prefix *is* the
corpus root, so `root_dir` (the `--dir` root) is ignored by the S3 backend and
does not have to exist. Only the **state directory** must be local
(`dirstral-spec/docs/SPEC.md` §7.8) — that is where SQLite, the index and the
caches live. `dir2mcp up` on an `s3` source therefore no longer fails with
`root inaccessible` when no local corpus is present, and `dir2mcp service
install` anchors the supervised daemon's working directory on the **directory
holding your config file** (where `.dir2mcp.yaml` and `.env.local` live) instead
of the ignored corpus root. `local`/`nfs` are unchanged: they still require an
accessible root directory and still boot the service in it.

`rel_path` (document identity) is stable across schemes: for `local`/`nfs` it is
the path under the root; for `s3` it is the object key minus the configured
prefix. A corpus can be relocated `local ⇄ nfs ⇄ s3` without forcing a reindex
on relocation alone. (`dirstral-spec/docs/SPEC.md` §7.8.)

### 2.4 Choose the vector backend (`index.backend`)

| `index.backend` | Tier | External infra | Search | When |
|---|:--:|:--:|---|---|
| `memory` (default) | A | none | Exact, exhaustive scan | In-memory, pure-Go, snapshotted to StateDir. Zero-infra default. |
| `disk` | B | none | Exact, exhaustive scan | Pure-Go on-disk / memmapped single-node index; for corpora too large for RAM. |
| `qdrant` | C | required | Approximate (ANN) | External Qdrant collection. |
| `pgvector` | C | required | Approximate (ANN), see note | External PostgreSQL + pgvector. |

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

#### `memory`/`disk` are exact search, not ANN

Despite the `HNSWIndex` type name in `internal/index/hnsw_index.go` (kept as-is
to avoid churn to a public type and its on-disk snapshot format), neither
Tier-A/B backend builds an HNSW graph or does approximate nearest-neighbor
search. Both `memory` and `disk` score every query vector against **every**
stored vector with an exhaustive cosine scan and keep only the top-k. There is
no ANN index to build, no recall/latency knob to tune, and no approximation
error: recall is exact, always identical to a brute-force scan over the whole
corpus. The trade-off is that query cost grows **linearly** with the number of
indexed chunks, because every query touches every vector.

As a rough order-of-magnitude guide (not a benchmark; actual numbers depend on
hardware, embedding dimensionality, and concurrent query load), expect the per-query cost of the exhaustive scan to become noticeable somewhere
around **100K–200K chunks** (roughly **0.5–4s per query** in that range) and to
reach multi-second latency around **~1M chunks**. If your corpus is at or
approaching that order of magnitude and query latency matters, switch
`index.backend` to `qdrant` or `pgvector`, which build real ANN indexes and stay
sub-linear as the corpus grows.

One caveat on `pgvector`: its `hnsw`/`ivfflat` indexes are capped at
**2000 dimensions** (`HNSWMaxDim`). Above that, dir2mcp skips the ANN index and
warns, and the table answers queries by **exact sequential scan**, so you are
back to linear cost with an external database in front of it. This is not a
corner case: `gemini-embedding-001` defaults to 3072 dimensions. If you need ANN
on `pgvector`, request a smaller Matryoshka output dimension (`<= 2000`) for the
embedding model, or use `qdrant`, which has no such limit.

### 2.5 Start the daemon and expose only the MCP endpoint

Bring up the daemon on the VPS with the corpus root and a local state directory.
Expose only the MCP listen address to clients; keep the inference ports bound to
loopback. If you expose the endpoint publicly, remember that `--public` requires
auth unless `--force-insecure` is explicitly set.

If you put a reverse proxy or a tunnel in front of a loopback-bound daemon,
configure the proxy to forward a loopback `Host` header. Without that the MCP SDK
answers `403 Forbidden: invalid Host header` to every request, including
`initialize`. The recipes for cloudflared, ngrok, nginx, Caddy and Traefik are in
[Reverse proxy and tunnel: the `Host` header](../README.md#reverse-proxy-and-tunnel-the-host-header).

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

## 4. Optional: distributed embedding (coordinator + workers)

Everything above runs embedding **inside the daemon process**. For a large corpus
or a slow embed model, you can instead let the daemon act as a **coordinator** that
enqueues embedding jobs onto a shared queue, and run one or more standalone
**`dir2mcp embed-worker`** processes that lease jobs, embed, and write vectors
directly to a shared store. This is opt-in (`distributed_embed.enabled: true`) and
governed by `dirstral-spec/docs/SPEC.md` §8.7.

This is a different axis from the single-machine "daemon on the GPU VPS" topology
above — you only need it when one embedder cannot keep up. The corpus-read and
StateDir rules from §1 still apply: corpus content is read where each worker runs,
and each process keeps its own local StateDir.

### 4.1 What this requires

Distributed mode has two hard prerequisites, both enforced at config load /
worker startup:

- **A shared Tier-C vector store.** `distributed_embed.enabled: true` requires
  `index.backend: qdrant` or `index.backend: pgvector`. The embedded Tier-A
  (`memory`) and Tier-B (`disk`) backends are single-node and cannot be shared
  across processes, so the daemon and `embed-worker` **reject** them with a
  `CONFIG_INVALID`-style error at startup (SPEC §8.7.4). All coordinator and
  worker processes must point at the **same** Tier-C collection/table.
- **A broker** that holds the job queue (below).

Every process — coordinator and all workers — must also share the **same embed
identity** (provider / model / dims; see §5). They are embedding into one vector
space, so their embed config must agree.

### 4.2 Broker (job queue)

Pick the broker with `distributed_embed.broker`. Only the two **built-in** brokers
are shipped today; external broker adapters (Redis, NATS, etc.) are **not yet
implemented** and are rejected with `unsupported distributed_embed.broker` if
named:

| `distributed_embed.broker` | Backing | Scope | When |
|---|---|---|---|
| `memory` (default) | in-process queue | single process only | Degenerate / testing; the queue lives inside one process and is not visible to separate worker processes. |
| `sqlite` | a SQLite queue file | multi-process on one host | Persistent, pure-Go, file-backed queue. Workers on the **same host** share it via `distributed_embed.sqlite_path` (defaults to `<state_dir>/embed-queue.db`). |

> The `memory` broker is in-process only, so it does **not** connect a separate
> `embed-worker` to the coordinator. The `sqlite` broker shares a queue **file**,
> which means coordinator and workers must run on the **same host** (or a host
> where they all see the same path). A broker that fans work out across *different*
> hosts needs an external broker adapter, which is on the SPEC §8.7 roadmap but not
> shipped — so today's distributed topology scales workers **on the GPU VPS
> itself**, not across machines.

```yaml
index:
  backend: qdrant
  qdrant:
    url: http://localhost:6334
    collection: my-corpus

distributed_embed:
  enabled: true
  broker: sqlite                 # memory | sqlite (built-ins only)
  sqlite_path: /var/lib/dir2mcp/embed-queue.db   # optional; defaults to <state_dir>/embed-queue.db
  max_attempts: 5                # optional; redelivery bound before dead-lettering (default 5)
```

> The runtime-only broker connection string for a future external broker is read
> **only** from the `DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL` environment variable and
> is never persisted to the config file. With only built-in brokers shipped today
> it has no effect.

### 4.3 Run the coordinator and the workers

1. **Coordinator** — start the daemon as usual (`dir2mcp up …`) with
   `distributed_embed.enabled: true`. It enumerates the corpus and enqueues embed
   jobs onto the broker instead of embedding inline.
2. **Workers** — on the same host, run one or more:

   ```bash
   dir2mcp embed-worker
   ```

   `embed-worker` serves no MCP traffic; it leases embed jobs, calls the (localhost)
   embed server, and writes vectors to the shared Tier-C store. It uses the same
   config file as the coordinator (same Tier-C store, same broker, same embed
   identity). Optional tuning flags:

   - `--lease-duration` — visibility timeout for a leased job (default `30s`).
   - `--poll-interval` — wait after an empty queue before leasing again (default `500ms`).
   - `--retry-after` — delay before a transiently-failed job is redelivered (default `2s`).

Run as many `embed-worker` processes as your GPU(s) can feed. Because all workers
write to the same Tier-C collection under the same embed identity, the result is a
single coherent index.

### 4.4 Sharing one broker between corpora

Two corpora may point at the same `sqlite_path`. Each corpus is identified by a
stable `corpus_id` (SPEC §5.5), derived on first use from the corpus root (or,
for `source.kind: s3`, from bucket + prefix + endpoint) and persisted in that
corpus's own metadata store. It is an opaque digest: it carries no path, bucket,
endpoint or credential, so a queue file readable by both corpora discloses
neither corpus's layout to the other.

Every job carries its `corpus_id`, and it governs routing end to end:

- the queue deduplicates jobs per corpus, so two corpora whose chunk ids collide
  (they are per-corpus SQLite rowids, so this is normal, not exotic) each keep
  their own job;
- a worker leases only jobs for the corpus its metadata store belongs to, and
  refuses to execute one for any other corpus even if a broker hands it over.

Moving or re-mounting a corpus does not change its `corpus_id` — the persisted
value wins over the derived one, so in-flight jobs and already-written vectors
stay bound to the corpus they came from.

### 4.5 When a job cannot succeed

A job is redelivered up to `max_attempts` times and then dead-lettered. On that
final attempt the chunk itself is recorded as `embedding_status=error` with a
category, so:

- it leaves the pending set and the coordinator stops minting new jobs for it —
  a permanent misconfiguration (mismatched embed identity, an index axis no
  worker serves) fails **bounded** instead of retrying forever;
- it appears in `dir2mcp status` and `dir2mcp doctor` under the error-category
  breakdown, alongside in-process embedding failures.

A failure that clears within the retry budget is simply redelivered and succeeds;
nothing is recorded. To retry a chunk that was recorded as failed — after fixing
the cause — re-pend it with `dir2mcp reindex`.

A job whose chunk changed after it was enqueued (re-ingested with different text,
or moved to the other index axis) is acknowledged as **superseded** without being
embedded, and the coordinator enqueues the chunk's current form on its next pass.

---

## 5. Caveats and current limitations

- **FUSE-over-S3 listing cost.** If you mount S3 as a filesystem (mountpoint-s3,
  goofys, etc.) instead of using `source.kind: s3`, directory listings over a
  large object tree can be slow and request-expensive, because a filesystem walk
  turns into many `LIST`/`HEAD` calls. Prefer the native `source.kind: s3`
  backend (a flat object listing) for large corpora; reserve FUSE for cases where
  you specifically need a real mountpoint. The same listing cost applies to a deep
  **NFS** tree. For a large local-filesystem-style backend (including NFS and FUSE
  mounts) where most directories are unchanged between runs, you can opt into the
  directory-discovery **scan cache** so an unchanged directory skips re-reading and
  re-sorting its entries:

  ```yaml
  ingest:
    scan_cache: true   # default OFF; opt-in. Local-filesystem backend only.
  ```

  The cache keys each directory on its own mtime plus its direct children's
  name/size/mtime/mode, so a changed directory is still rescanned. A directory
  that is being written to while the scan runs keeps the full read, so a corpus
  under active write gains less from the cache than a settled archive. The cache
  is only consulted for the local-filesystem backend (not the native
  `source.kind: s3` object listing).

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

## 6. Related docs

- [`SPEC.md`](SPEC.md) — pointer to the canonical, normative spec (this guide's
  references resolve there): §6 (vector backends), §7.8 (remote corpus), §8.5
  (self-hosted endpoints), §8.7 (distributed embedding), §16.1.1 (credentials),
  §16.2 (config schema).
- [`VISION.md`](VISION.md), [`ECOSYSTEM.md`](ECOSYSTEM.md) — product and ecosystem context.
- [`x402-payment-adapter-spec.md`](x402-payment-adapter-spec.md) — request-gating
  adapter contract, if you gate the exposed MCP endpoint.
