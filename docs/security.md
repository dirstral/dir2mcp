# Security defaults

## Security Defaults

- Default listen address is local (`127.0.0.1:0`)
- `--public` binds to `0.0.0.0` (unless explicit `--listen` is provided)
- `--public` with `--auth none` is rejected unless `--force-insecure` is set
- Browser origins are allowlisted (localhost defaults + explicit additions)
- By default the server does not index its own configuration: `.dir2mcp.yaml`, `.env` and `.env.local` are in the default `security.path_excludes` (with `.git/`, `.dir2mcp/`, keys and certificates), so a key in any of them does not become a searchable or citable document. A `security.path_excludes` list in your config replaces the defaults, so copy these patterns into it when you set one. (`.env` and `.env.*` files are also skipped by classification, whatever the list says; `.dir2mcp.yaml` depends on the list.)
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

### Secret patterns: documents withheld for their content

`security.secret_patterns` is a list of regular expressions. Ingest tests
each document against it: the raw bytes, and every text that dir2mcp derives
(extraction, OCR, transcript, translation). A match withholds the whole
document. Its status is `secret_excluded`, and `search`, `ask` and
`open_file` do not return it. For ingest, a list in your config replaces the
defaults.

What "the raw bytes" means depends on the document type:

- **Text, code, markdown, data and HTML files:** ingest checks every byte.
  These bytes are the searchable text.
- **Every other type (PDF, image, office document, audio, video):** ingest
  checks only the first 64 KiB of the raw bytes. The rest of the file is not
  checked as bytes. Do not conclude that dir2mcp finds a credential anywhere
  in such a file: bytes after the first 64 KiB that never become text (for
  example metadata or an attachment inside a container) are not checked.
  What ingest checks in full is every text that it derives from the file:
  extracted text, OCR text, the transcript, a subtitle sidecar, a
  translation, an annotation. Only derived text becomes searchable.
- **`ingest.media_identity`** does not change this for audio and video. With
  `full`, ingest reads the whole file for its content hash, and checks the
  first 64 KiB. With `sampled`, ingest reads three slices of a large file for
  its identity, and checks the first 64 KiB of the first slice. In both
  modes the transcript, the sidecar text and the translations are checked in
  full.

The defaults (SPEC §7.2, spec 0.97.0):

| Rule | Pattern |
| --- | --- |
| AWS access key ID | `AKIA[0-9A-Z]{16}` |
| AWS secret assignment | `(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?\|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}` |
| JWT with an auth context | `(?i)(?:authorization\s*[:=]\s*bearer\s+\|(?:access\|id\|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}` |
| Generic bearer token | `(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}` |
| `sk_` and `api_` keys | `(?:^\|[^A-Za-z0-9_])(?:sk_[a-z0-9]{32}\|api_[A-Za-z0-9]{32})` |
| `sk-` keys (OpenAI, Mistral) | `(?:^\|[^A-Za-z0-9_-])sk-[A-Za-z0-9-]{20,}` |
| Provider key assignment | `[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}` |

The generic token rule matches any key that ends in `token` with a value of
20 or more key characters. So it catches the credential keys of every
provider (`GITHUB_TOKEN=`, `VERCEL_TOKEN=`, `DISCORD_TOKEN=`, `id_token:`).
A rule that lists credential key names would miss every provider that the
list does not name, so the rule stays broad. The three key rules require the
key to start a word, so the `sk-` inside a URL slug such as
`krasnoyarsk-...` and the `sk_` inside an identifier such as `task_...` or
`task_sk_...` do not match. For the `sk_` and `api_` rule, an underscore
before the prefix does not start a word.

This boundary has a cost, a known miss: a key that directly follows an
underscore (`backup_sk_<32 characters>`, `backup_sk-<20 characters>`) matches
no default. One left boundary cannot tell `task_sk_` from `backup_sk_`. The
same key after a space, a quote or `=` matches. If you store keys under such
names, add a pattern for them to `security.secret_patterns`, with the
defaults, for example:

```yaml
security:
  secret_patterns:
    # ... the defaults ...
    - 'backup_sk[_-][A-Za-z0-9-]{20,}'
```

### Allowed matches (`security.secret_allow_patterns`)

The generic token rule also matches values that carry no credential, such as
the `redir_token=` parameter of a YouTube redirect link that a video
description pastes. `security.secret_allow_patterns` releases such text. The
default list holds one pattern:

`(?i)(?:^|[\s"'<>(\[])https?://(?:www\.|m\.)?youtube\.com/redirect\?(?:[^\s&#<>"']*&)*?(redir_token=[^\s&#;?=:<>"']*)`

It names the host and the path, not only the parameter. A parameter name
alone does not show that its value is not a credential, so the default trusts
no name outside that one link. To release another benign parameter (for
example a pagination cursor of an API you use), add your own pattern, anchored
to its host.

**The cover.** An allow pattern has a cover, and only the cover cancels:

- When the pattern has a capture group, the cover is the text of its **first**
  capture group. The rest of the match is context: it must be present, but it
  cancels nothing.
- When the pattern has no capture group, the cover is the whole match.
- Use `(?:...)` for a group that must not narrow the cover. A plain `(...)`
  that comes first becomes the cover, also when you wrote it only to group an
  alternative. Then the pattern can cancel less than you expect, never more.
- A first group that takes no part in a match (an optional group, or a group
  in an alternative that did not match) gives that match no cover.

The default pattern uses this. Its context is the start of a YouTube redirect
URL. The URL must begin the text or follow a space, a line break, a quote, an
angle bracket, `(` or `[`, so the link is not a part of the path of another
URL. Its cover is only the `redir_token` assignment, so a credential in
another parameter of the same link is not cancelled.

dir2mcp finds every match of every secret pattern. It looks at every start
position, so a match that starts inside a cancelled match is still found and
judged on its own. A cover cancels a match of the generic token rule when it
covers it: the cover starts at or before the start of the secret match and
ends at or after its end. A cover never cancels a match of any other rule (an
AWS key, an `sk-` key, your own rule), also when it covers it. A text is
withheld when at least one secret match is left. So, for a YouTube redirect
link:

- A redirect link alone does not withhold a document.
- A redirect link and a real credential in one document withhold it.
- `redir_token=abc;GITHUB_TOKEN=...` withholds it: the allow value stops at
  `;`, so it does not cover the credential.
- `redir_token=abc?GITHUB_TOKEN=...` and `redir_token=abc,GITHUB_TOKEN:...`
  withhold it: the allow value also stops at `?`, `=` and `:`, so it never
  covers a second assignment.
- `redir_token=<20 characters>GITHUB_TOKEN=...` withholds it: the second
  assignment starts inside the cancelled match and is judged on its own.
- `'https://youtube.com/redirect?x=1'&redir_token=...` withholds it: the
  parameters and the value stop at a single or double quote, so an assignment
  after the closing quote of a quoted link is not covered.
- `redir_token=AKIA...` withholds it: an allow match never cancels an AWS key.
- `youtube.com/redirect?GITHUB_TOKEN=...&redir_token=x` withholds it: only
  the `redir_token` assignment is covered.
- `https://evil.example/https://www.youtube.com/redirect?...&redir_token=...`
  withholds it: the link is inside the path of another URL.
- `page_token=...` or `redir_token=...` on any other host withholds it.

The allow list applies wherever the secret patterns apply: source bytes,
derived text, and the text a tool returns. An empty list cancels nothing. An
allow pattern that is not a valid regular expression is a `CONFIG_INVALID`
error.

### When a pattern list changes

When the effective lists change (a new version with new defaults, or an edit
to your config), the next scan applies them in both directions. You do not
need to run `dir2mcp reindex`.

- **Withheld documents are evaluated again.** At the start of the scan, dir2mcp
  clears the content hash of each `secret_excluded` document, and of each
  archive that holds one. The normal pipeline then reads them again. A document
  that the new lists do not match is indexed. A document that they still
  match stays withheld.
- **Indexed documents are evaluated again when the change can match more.**
  A change can match more when it adds or edits a secret pattern, or removes
  or edits an allow pattern. Then dir2mcp also clears the content hash of each
  indexed document and each archive, and marks each one as waiting for
  re-evaluation. The scan then reads each of them again: the source bytes and
  the text that dir2mcp derives (extraction, OCR, transcript, translation). A
  document that the new lists match is withheld, and its representations and
  chunks are retired. OCR, transcription and translation come from their
  on-disk caches, so this does not repeat a provider call that already
  succeeded. But it reads every indexed file once, which takes time on a large
  media corpus. With `media.batch.two_phase`, the transcription pass retires
  the translations of a marked document before any other step reads them. The
  derivation pass then makes them again from the cache and checks them.
- **Document summaries use checked text only.** Before dir2mcp sends a
  representation to the summary model, it checks the text against the active
  lists, and it checks the summary that comes back. A match in either sends
  nothing more and keeps no summary, also not in the summary cache: the
  document is withheld as `secret_excluded`, as for any derived text that
  matches.
- **Between the change and the start of the scan, no tool returns any indexed
  document.** The scan records the new lists and marks the documents at its
  start. A server can answer before that (`up` starts to serve before its
  first scan, and `ask` runs no scan). So on each request dir2mcp compares the
  recorded lists with the configured ones. When the configured lists can
  match more, it withholds every indexed document until the scan records them.
- **Until the scan has read a marked document again, no tool returns any part
  of it.** `search` and `related` leave out its hits, `ask` leaves out its
  passages before it builds the prompt, and `open_file` returns `FORBIDDEN`.
  `related` returns `FORBIDDEN` when the seed chunk or seed document is such
  a document, and it does not send the seed text to the embedder. A hosted
  reranker does not receive the text of such a document. When dir2mcp cannot
  read the pending marks, it sends no candidate text to the reranker and the
  request fails. A check of each excerpt is not enough here, because a
  credential can cross a chunk boundary and then no single excerpt matches.
- **Only the current lists release a marked document.** Each scan writes its
  documents with the fingerprint of the lists it checked them with. The store
  clears a mark only when that fingerprint equals the recorded one. A scan
  that started before the last change cannot release a document, and a
  document it writes is marked for the next scan. A marked document also stays
  marked through a later change, also one that can only match less, until a
  scan checks it against the lists that are current then.
- **A tool uses text only from the document version that was checked.** The
  search index can hold the snippet of an earlier version of a chunk. Before
  `search`, `ask` and `related` return or send chunk text, they take the text
  from the stored chunk, and they compare the document's status and content
  hash before the read and after the check for marks. When the document
  changed in between, its hits are left out of that request.
- **A change that can only match less skips that read and hides nothing.**
  Only three changes count: a removed secret pattern, an added allow pattern,
  and the shipped narrowings of the `sk-`, `sk_` and `api_` rules. Any other
  edit counts as a change that can match more, also one that is stricter in
  fact. The test is on the matches that stay after an allow pattern cancels
  some, not on one list alone. An allow pattern cancels only a match of the
  generic token rule. If you replace that rule with your own token pattern,
  also a narrower one, no allow pattern cancels its matches, so the change
  can match more and the scan reads every indexed document again.

The check does not use the stored chunks. A chunk can lose bytes that the raw
scan sees (for example a trimmed trailing newline), and chunks can overlap, so
only a new read gives the same verdict that ingest gives.

Each tool that returns indexed text also tests that text against the lists in
effect when it answers:

- `search` and `related` leave out a hit whose snippet or chunk text matches.
  `related` also does not send matching seed text to the embedder, and a
  hosted reranker does not receive matching candidate text.
- `ask` does not place a matching chunk in the prompt, and does not cite it.
- `open_file` checks the whole source before it returns any part of it, and
  returns `FORBIDDEN` when the source matches. This is the same check ingest
  makes on the same bytes. It reads the source once for each call, so the
  part it returns comes from the bytes that it checked; a file that changes
  during the call cannot add content that no check covered. This also applies
  when the index holds page or time chunks for a text file: the whole file is
  checked first. For a document whose text is derived (a PDF, an image, audio,
  video, an office document), a page or a transcript excerpt comes from the
  stored chunks, and the check covers that excerpt. The text that `open_file`
  returns is checked once more after it is cut to `max_chars`: a cut can
  remove the end of an allow match, and the call then returns `FORBIDDEN`.
  The read is
  bounded by `ingest.max_file_mb`. A
  source over that cap returns `FILE_TOO_LARGE`: ingest never checked or
  indexed it.
- `annotate` and `transcribe` return `FORBIDDEN` for matching text.

These tools also apply every shipped default secret pattern when your list
leaves one out. So a tool can refuse text that ingest indexed under your
list.

The store records both lists and their fingerprint (`secret_policy_patterns`,
`secret_policy_allow_patterns` and `secret_policy_identity` in the settings
table), in the same transaction that clears the content hashes. A document
keeps its empty content hash, and its mark, until the scan has decided it
again, so a document that fails, or that a stopped scan did not reach, is
processed again on the next scan and stays hidden until then. If the clear
itself fails, the scan fails and records nothing.

A corpus with no recorded lists was indexed by a version before spec 0.97.0.
For such a corpus the earlier lists are unknown. dir2mcp does not assume the
default lists, and it does not use the effective-config snapshot
(`<state>/.dir2mcp.yaml.snapshot`) as a record: every command that loads the
config rewrites that file, also a command that does not scan, so it can hold
lists that never checked the corpus. So the first scan after the upgrade
reads every indexed document again, and no tool returns a document until that
scan has decided it. This takes one full read of the corpus (OCR,
transcription and translation come from their caches). Documents come back
one by one as the scan decides them. After that scan the lists are recorded,
and a later change that can only match less reads only the withheld
documents.

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
