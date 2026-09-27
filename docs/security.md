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
