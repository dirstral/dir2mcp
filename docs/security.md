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
`open_file` do not return it. A list in your config replaces the defaults.

The defaults (SPEC §7.2, spec 0.93.0):

| Rule | Pattern |
| --- | --- |
| AWS access key ID | `AKIA[0-9A-Z]{16}` |
| AWS secret assignment | `(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?\|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}` |
| JWT with an auth context | `(?i)(?:authorization\s*[:=]\s*bearer\s+\|(?:access\|id\|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}` |
| Generic token assignment | `(?i)(?:^\|[^a-z0-9_-])(?:(?:[a-z0-9]+[_-])*(?:access\|auth\|api\|bearer\|secret\|private\|session\|refresh\|personal\|client\|oauth\|github\|gh\|gitlab\|slack\|npm\|pypi\|hf\|bot)[_-]?)?token\s*[:=]\s*[A-Za-z0-9_.-]{20,}` |
| `sk_` and `api_` keys | `(?:^\|[^A-Za-z0-9])(?:sk_[a-z0-9]{32}\|api_[A-Za-z0-9]{32})` |
| `sk-` keys (OpenAI, Mistral) | `(?:^\|[^A-Za-z0-9_-])sk-[A-Za-z0-9-]{20,}` |
| Provider key assignment | `[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}` |

The generic token rule matches `token`, or `token` after a credential word
(for example `GITHUB_TOKEN=`, `X-Auth-Token:`, `access_token=`,
`MY_APP_SECRET_TOKEN=`). It does not match a key name that only ends in
`token`, such as `redir_token=` in a YouTube redirect link, `next_page_token=`
or `csrf_token=`. For the same reason it does not match a service name that is
not in its list, such as `DISCORD_TOKEN=`. If your corpus can hold such
credentials, add a rule for them to your list. The three key rules require the key to start a word, so the
`sk-` inside a URL slug such as `krasnoyarsk-...` and the `sk_` inside an
identifier such as `task_...` do not match. Before spec 0.93.0 these rules had
no boundary, and documents that quoted such links were withheld.

When the effective list changes (a new version with new defaults, or an edit
to your config), the next scan evaluates every withheld document again. You do
not need to run `dir2mcp reindex`. The store records a fingerprint of the list
(`secret_policy_identity` in the settings table). When the fingerprint
changes, the scan clears the content hash of each `secret_excluded` document,
and of each archive that holds one, so the normal pipeline reads them again. A
document that no longer matches is indexed; a document that still matches
stays withheld. Documents that are not withheld are not read again for this
reason. So when you add a pattern, run `dir2mcp reindex`: an incremental scan
does not apply a new rule to every document that is already indexed (for
example a media file or an S3 object that the fast path skips, text that
dir2mcp derived from a document, or an archive member).

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
