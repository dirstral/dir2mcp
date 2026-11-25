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

### `dir2mcp_open_file` on media with no text

`dir2mcp_open_file` returns text only. Two cases have no text to return, and
both answer the non-retryable `MEDIA_NO_TEXT`:

- a media document that is embedded directly and has no text representation
  (`model.embed.multimodal: replace`);
- a recording that the scan skipped as `no_speech` (`status=skipped`,
  `skip_reason=no_speech`): the speech-to-text decode found no speech in the
  selected audio tracks, so there is no transcript. This holds for audio and
  video, and for each span kind (no span, lines, page, time). The tool does
  not return the retryable `OCR_NOT_READY` here, and it does not return empty
  content.

The same holds for a document that stays `status=ok` through another
representation (recognition, for example) after every selected audio track was
a no-speech result. Its transcript is absent, not pending. A call that would
return the transcript returns `MEDIA_NO_TEXT`: the default call or a line span
on an audio or video file, and a time span that no other representation answers. A call
that another representation answers is answered from it, for example
recognition text for a time span. A document in which one selected track has a
transcript and another track holds no speech is not this case: the tool
returns its transcript.

### `dir2mcp_transcribe` and `dir2mcp_transcribe_and_ask` on a recording with no speech

A recording that holds no speech is a success for both tools, not an error.
`dir2mcp_transcribe` returns `indexed: false`, `transcribed: false` and no
segments.

- `retranscribe: false` (the default): when a no-speech result is stored for
  the recording under the current speech-to-text identity (provider, model,
  language settings), the tool sends no decode request.
- `retranscribe: true` is an explicit request, so the tool always decodes the
  recording again. When the new decode again finds no speech, the result is
  the same. An earlier transcript of the recording is then retired with its
  chunks, its translations and its summaries, and the document is in the
  state that a scan writes: `status=skipped` with `skip_reason=no_speech`
  when it has no other searchable representation.
- The tools decode the first audio track only. When `media.stt.tracks`
  selects more tracks, a first track with no speech does not settle the
  document as `no_speech`. The tool withholds the done marker, so the next
  scan decodes every selected track and decides.
- `transcribed_now` is `true` only when the call sent a decode request.
- `dir2mcp_transcribe_and_ask` has no `retranscribe` argument. It behaves as
  `retranscribe: false`.

A client must not retry on `MEDIA_NO_TEXT`. `OCR_NOT_READY` is the retryable
answer: the text does not exist yet, and a later call can succeed.

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

### Editorial annotations in results

An imported editorial annotation (`dir2mcp annotate import`, SPEC §7.11) is its
own hit with `rep_type: editorial`. It is an editor's note from the named
source, not words spoken in the media and not model output. Its span carries:

| Field | Meaning |
|---|---|
| `event` | `appears` (a person), `segment` (a timecoded segment) or `description` (the document-level text) |
| `entities` | on a person hit, the normalized person id |
| `sources` | `[<import source name>]` |
| `attributes` | `display_name` and `role` on a person hit; on every hit the match provenance (`match_score`, `external_id`, `external_name`, `external_date`, `match_<member>`) and each extra field of the record |

A person or segment hit has a `time` span when the media duration is known. A
description hit has a `document` span, which carries the same four fields. An
editorial hit has no `derivation`.

Filters on editorial hits:

- `entities`: a requested value is normalized like a stored person id (see
  [`annotations.name_folds`](configuration.md#editorial-person-ids-annotationsname_folds))
  before the comparison. Against a recognition hit the comparison stays
  literal.
- `events`, `attributes`: as for recognition hits, for example
  `attributes: {"content_id": ["C-1001"]}`.
- `sources` (`dir2mcp_search`, `dir2mcp_ask`, SPEC §9.11): keeps only
  annotation hits whose span `sources` contains a requested value. With the
  import source name it selects that source's hits. A recognizer tag is in the
  same namespace, so do not name an import source after a recognizer tag of the
  same corpus, and check `rep_type: editorial` when you must isolate editorial
  hits.

A citation of an editorial hit names the source and the score, for example
`[news.mp4@t=00:04-01:12 › editor annotation (example-catalogue, match 1.25)]`
or `[news.mp4 › editor annotation (example-catalogue)]` without a score.

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
