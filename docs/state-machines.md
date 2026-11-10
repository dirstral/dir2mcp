# State machines

<!-- Generated from internal/model/lifecycle_status.go. Do not edit by hand.
     Regenerate: DIR2MCP_UPDATE_STATE_MACHINES_DOC=1 go test ./tests/lifecycle -run StateMachinesDoc -->

dir2mcp keeps two persisted lifecycles: the status of a document and the
embedding status of a chunk. This page lists their states, the transitions
the code can make today, and where each state is visible.

The tables record what the code does. They forbid nothing yet. A write
that the table does not list is logged as
`lifecycle: unlisted transition <entity> <id> <from>-><to>` and then goes
ahead unchanged.

## Document status (`documents.status`)

States: `ok`, `skipped`, `error`, `secret_excluded`, `pending`.

The store folds any other value to `ok` (`NormalizeDocumentStatus`).
No code path writes `pending` today; the store keeps it when written.

### Transitions

| From | To | Via |
| --- | --- | --- |
| (new row) | `ok` | scan build (buildDocumentWithContent), archive member, MCP transcribe upsert |
| (new row) | `skipped` | scan build (archive, binary_ignored, ignore), size_cap, symlink, watch skip |
| (new row) | `error` | persistBuildError (unreadable source) |
| (new row) | `secret_excluded` | raw-byte secret match at build |
| `ok` | `ok` | re-scan, finalizeContentHash (done marker) |
| `ok` | `skipped` | persistNonFatalDocSkip, watch skip, size_cap, re-classified type |
| `ok` | `error` | representation failure, persistNonFatalDocError |
| `ok` | `secret_excluded` | derived-text secret match (secret_derived.go) |
| `skipped` | `ok` | re-scan of an eligible file |
| `skipped` | `skipped` | re-scan, archive finalize |
| `skipped` | `error` | archive extraction failure |
| `skipped` | `secret_excluded` | re-scan with a secret match |
| `error` | `ok` | retry on the next scan |
| `error` | `skipped` | re-scan builds a skip row |
| `error` | `error` | retry fails again |
| `error` | `secret_excluded` | retry with a secret match |
| `secret_excluded` | `ok` | content changed or --force, no secret now |
| `secret_excluded` | `skipped` | re-scan builds a skip row |
| `secret_excluded` | `error` | re-scan fails |
| `secret_excluded` | `secret_excluded` | carrySecretExclusion, finalizeSecretExcluded |
| `pending` | `ok` | any rebuild (no writer produces pending) |
| `pending` | `skipped` | any rebuild |
| `pending` | `error` | any rebuild |
| `pending` | `secret_excluded` | any rebuild |

### Where it is visible

- MCP `list_files` field `status`, enum `ok`, `skipped`, `pending`, `error`. Projection:

| Stored | `list_files` |
| --- | --- |
| `ok` | `ok` |
| `skipped` | `skipped` |
| `error` | `error` |
| `secret_excluded` | `skipped` |
| `pending` | `pending` |
| any other value | `ok` |

- Persisted corpus stats (`CorpusStats`, read by the `stats` tool and
  `dir2mcp status`), live documents only:
  `indexed` counts `ok`; `skipped` counts `skipped` and `secret_excluded`;
  `errors` counts `error`; `deleted` counts tombstoned rows of any status.
- `skip_summary` groups `skipped` and `secret_excluded` rows by `skip_reason`.

## Chunk embedding status (`chunks.embedding_status`)

States: `pending`, `ok`, `error`.

The store folds any other value to `pending` (`NormalizeEmbeddingStatus`).

### Transitions

| From | To | Via |
| --- | --- | --- |
| (new row) | `pending` | chunk insert (represent.go), UpsertChunkTask |
| (new row) | `error` | quality-gate quarantine at insert |
| `pending` | `pending` | re-ingest chunk upsert, UpsertChunkTask |
| `pending` | `ok` | MarkEmbedded |
| `pending` | `error` | MarkFailed, MarkFailedWithCategory, quarantine on re-ingest |
| `ok` | `pending` | RependEmbeddedChunks (reconcile), re-ingest chunk upsert |
| `ok` | `ok` | MarkEmbedded again (unconditional UPDATE) |
| `ok` | `error` | quarantine on re-ingest, MarkFailed (unconditional UPDATE) |
| `error` | `pending` | RequeueFailedChunks, re-ingest chunk upsert |
| `error` | `ok` | MarkEmbedded (unconditional UPDATE) |
| `error` | `error` | MarkFailed again, quarantine on re-ingest |

### Where it is visible

- Persisted corpus stats (`CorpusStats`), live chunks only:
  `embedded_ok` counts `ok`; `embedded_pending` counts `pending`;
  `failed_chunks` and the failure summary group `error` rows by `error_category`.
- The embed worker reads `pending` rows (`NextPending`). BM25 and the
  summary paths leave out `error` rows.
- `dir2mcp reindex --embeddings-only` moves `error` rows back to `pending`.
