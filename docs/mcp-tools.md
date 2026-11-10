# MCP tools and citations

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
| `dir2mcp_stats` | Corpus statistics, including the extraction engine actually in use and the `evidence` thresholds in effect with their null baseline (see [configuration](configuration.md#relevance-floor-and-insufficient-evidence-abstention)) |
| `dir2mcp_recheck_path` | Reconcile one file or directory under the root now (optional; served only in watch mode, see below) |

### `dir2mcp_recheck_path` (optional)

A server that runs the filesystem watcher (`ingest.watch: true` on a `local` or
`nfs` source, not `--read-only`) also serves `dir2mcp_recheck_path`. A client
that knows a path changed asks the server to reconcile that path now, instead
of at the next periodic safety rescan. The typical case is an NFS corpus that
another host writes to: the watcher gets no event for such a write (see
[configuration](configuration.md#continuous-incremental-indexing-optional)).

| Argument | Meaning |
|---|---|
| `rel_path` (required) | A path relative to the corpus root, with the grammar of `dir2mcp_open_file`'s `rel_path`. A file or a directory. `.` names the root |
| `recursive` (default `true`) | For a directory, reconcile the whole subtree. With `false`, only the regular files that are its direct children. Ignored for a file |

The call validates the path, stats it once, queues the reconcile and returns at
once. It does not wait for ingestion. The response carries exactly four fields:

| Field | Meaning |
|---|---|
| `rel_path` | the normalized path (`.` for the root) |
| `kind` | what the stat found at call time: `file`, `directory` or `missing` |
| `queued` | `true` when this call added a request; `false` when a request for the same path was already pending (the queue holds one per path; a recursive repeat makes a pending non-recursive request recursive) |
| `pending` | the number of requests pending after this call |

The watcher's worker then applies the rules of a filesystem event for the path
as it finds it then: a changed file is re-indexed and an unchanged one is left
alone; every file under a directory is reconciled and a document whose file is
gone is tombstoned; a path that is still missing retires the document at it and
every document under it; an excluded path (ignore list, `security.path_excludes`,
`.gitignore`, secret patterns) is accepted and retired as ineligible.

Errors: `MISSING_FIELD` (empty `rel_path`), `INVALID_FIELD`, `PATH_OUTSIDE_ROOT`.
A server that does not run the watcher does not list the tool, and a call that
names it anyway fails with `TOOL_UNAVAILABLE` (non-retryable for the lifetime of
the process).

### What a citation carries

A `time` span (recognition and media content) carries more than its bounds, and a
client can show all of it:

| Field | Meaning |
|---|---|
| `start_ms` / `end_ms` | the span bounds, so a client can play exactly the cited moment |
| `event` | the structured event the annotation records, for example `home_run`. Filterable |
| `entities` | the entities the annotation names, for example `player:sam-rivera`. Filterable |
| `sources` | which recognizer produced the annotation, for example `["playbyplay"]` or `["scorebug","face"]`. Provenance only, never a ranking signal |
| `derivation` | `observed` or `generated`. A client MUST NOT present a `generated` span as a record of what happened |

`sources` and `derivation` answer different questions. `derivation` says whether a
span RECORDS or DESCRIBES. `sources` says WHICH component produced it, which
matters when two recognizers both observed and disagree. Both are optional and
omitted when absent, never served as `null` or `[]`.

### Inline citation tags in an answer

An `ask` answer cites inline with the bracketed tag of the context block the
statement comes from, for example `[notes.md:L25-L33]` or
`[interview.mp4@t=02:13-02:41]`. The server checks every tag against the blocks
the model was shown (SPEC §9.4.1):

- A tag that names a document the model was not shown is removed.
- A tag that names a shown document with a span outside the shown span is
  rewritten to the part of the shown span it overlaps, or to the whole shown span
  when it overlaps none of it. A span inside the shown span stays as the model
  wrote it.
- A tag with no span (`[notes.md]`) stays.

An inline tag therefore names only a span that was in the model's context. The
`citations` array is not affected by this; it always carries the real span of
each shown block.
