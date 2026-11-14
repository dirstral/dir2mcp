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

The defaults (SPEC §7.2, spec 0.93.0):

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

### Allowed matches (`security.secret_allow_patterns`)

The generic token rule also matches values that carry no credential, such as
the `redir_token=` parameter of a YouTube redirect link that a video
description pastes. `security.secret_allow_patterns` releases such text. The
default list holds one pattern:

`(?i)https?://(?:www\.|m\.)?youtube\.com/redirect\?(?:[^\s&#<>"]*&)*?redir_token=[^\s&#;?=:<>"]*`

It names the host and the path, not only the parameter. A parameter name
alone does not show that its value is not a credential, so the default trusts
no name outside that one link. To release another benign parameter (for
example a pagination cursor of an API you use), add your own pattern, anchored
to its host.

dir2mcp finds every match of every secret pattern. It looks at every start
position, so a match that starts inside a cancelled match is still found and
judged on its own. An allow match cancels a match of the generic token rule
when it covers it: the allow match starts at or before the start of the secret
match and ends at or after its end. An allow match never cancels a match of
any other rule (an AWS key, an `sk-` key, your own rule), also when it covers
it. A text is withheld when at least one secret match is left. So, for a
YouTube redirect link:

- A redirect link alone does not withhold a document.
- A redirect link and a real credential in one document withhold it.
- `redir_token=abc;GITHUB_TOKEN=...` withholds it: the allow value stops at
  `;`, so it does not cover the credential.
- `redir_token=abc?GITHUB_TOKEN=...` and `redir_token=abc,GITHUB_TOKEN:...`
  withhold it: the allow value also stops at `?`, `=` and `:`, so it never
  covers a second assignment.
- `redir_token=<20 characters>GITHUB_TOKEN=...` withholds it: the second
  assignment starts inside the cancelled match and is judged on its own.
- `redir_token=AKIA...` withholds it: an allow match never cancels an AWS key.
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
  nothing more and keeps no summary: the document is withheld as
  `secret_excluded`, as for any derived text that matches.
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
- **A change that can only match less skips that read and hides nothing.**
  Only three changes count: a removed secret pattern, an added allow pattern,
  and the shipped narrowings of the `sk-`, `sk_` and `api_` rules. Any other
  edit counts as a change that can match more, also one that is stricter in
  fact. The spec 0.93.0 defaults can only match less than the earlier
  defaults, so an upgrade with the default lists reads only the withheld
  documents again.

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
  makes on the same bytes. The read is bounded by `ingest.max_file_mb`. A
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

A corpus with no recorded lists was indexed by a version before spec 0.93.0.
dir2mcp does not assume that it used the default lists: your config shows the
lists of this run, not of the earlier one. Earlier versions wrote the lists
into the effective-config snapshot (`<state>/.dir2mcp.yaml.snapshot`) on each
start. Before the first 0.93.0 run replaces that snapshot, dir2mcp keeps its
lists once in `<state>/secret-policy-previous.json`. When those lists show
that the move can only match less (for example the earlier defaults to the
new defaults), dir2mcp reads only the withheld documents again and hides
nothing. With no such file, or when the move can match more, the earlier
lists count as unknown or broader: dir2mcp reads every indexed document
again, and withholds each one until the scan has decided it.

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
