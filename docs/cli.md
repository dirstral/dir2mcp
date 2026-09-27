# dir2mcp command reference

## CLI Commands

| Command | Description |
|---|---|
| `up` | Start the MCP server and begin indexing (daemonizes by default) |
| `down` | Stop the dir2mcp server running in this directory |
| `status` | Show corpus and indexing state. The counters (`reps`, `chunks`, `embedded`, `pending`, `errors`) are read from the metadata store on every call, so they are corpus-wide and match what the `dir2mcp_stats` MCP tool reports for the same state dir. If the store cannot be read, `status` reports the daemon's last cached counters instead and says so on stderr (`reporting cached counters`); `source` in `--json` is `computed` for a fresh read and `corpus_json` for the cache |
| `ask "<question>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `search "<query>"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `open-file <rel-path>` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `list-files` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |
| `reindex` | Force full re-ingestion. `--embeddings-only` instead retries just the chunks that failed to embed (see [Recovering from a failed embed run](#recovering-from-a-failed-embed-run)) |
| `embed-worker` | Run a standalone distributed embed worker (no MCP serving; requires a Tier-C store + broker) |
| `export` | Render a transcript as VTT/SRT/TTML subtitles (`export --format vtt\|srt\|ttml <path>`) |
| `bridge` | Run helper adapters (for example the ElevenLabs webhook bridge) |
| `support-bundle` | Collect logs + config + status into a shareable `tar.gz` (owner-only; credentials always redacted, local paths/endpoints redacted unless `--include-content` — see [What a support bundle discloses](security.md#what-a-support-bundle-discloses)) |
| `config init` | Interactive setup wizard (on a TTY). It first asks how to run the models. **Locally with Ollama**: it probes Ollama (`OLLAMA_HOST`, else `127.0.0.1:11434`), lists the installed embedding and chat models, asks for no key, and binds a `local` provider. **With a cloud key**: pick Mistral, OpenAI or Gemini and paste that one key (each gives embeddings and answers alone), optionally more providers, and where to store keys (`.env.local` or the OS keychain). Then a corpus profile. It writes `.dir2mcp.yaml` when it does not exist. An existing `.dir2mcp.yaml` is never rewritten: credentials still go to `.env.local` or the keychain, and a chosen profile is printed as the lines to add. Non-interactive (`--non-interactive`/`--json`/`--quiet`/no TTY) just writes a baseline config when none exists. `dir2mcp up` also launches this wizard on first run when started interactively (a TTY, and not `--json`/`--non-interactive`/read-only) and no embedding provider resolves. |
| `config print` | Print effective config |
| `config set-secret <ENV_VAR>` | Store a provider credential in the OS keychain (encrypted at rest) instead of a plaintext `.env.local` |
| `config rm-secret <ENV_VAR>` | Remove a credential from the OS keychain |
| `config secrets` | Show which provider credentials are present in the keychain / environment (never prints values) |
| `install <client>` | Install dir2mcp into a supported MCP client: `claude-code`, `cursor`, or `claude` (Claude Desktop). See [Connect an MCP client](#connect-an-mcp-client) |
| `uninstall <client>` | Remove dir2mcp from a supported MCP client. Other servers in the client config stay as they are |
| `doctor [<client>]` | With a client name, run client-integration diagnostics. With no argument, run a server-side preflight (config, provider resolution, an **egress** check reporting whether any resolved provider is a public/third-party host, extractor availability, indexing failures); add `--deep` to actively probe the embedding credential |
| `print-config <client>` | Print what a client needs for a manual setup: the `claude mcp add-json` command for `claude-code`, the `mcp.json` snippet for `cursor` and `claude`. For `claude-code` and `cursor` the output never contains the token |
| `service install\|uninstall\|status` | Auto-start the daemon at login so the corpus survives a reboot (macOS launchd) |
| `version` | Print version |

Running `dir2mcp` with no arguments, or any command with `--help` (or `-h`), prints usage to stdout and exits 0.
`ask`, `search`, `open-file`, and `list-files` are legacy compatibility shims; new client/orchestrator UX belongs in `dirstral-cli`.

### Connect an MCP client

Start the daemon first (`dir2mcp up`). Each client command reads the URL and the
bearer token from `.dir2mcp/connection.json` and `.dir2mcp/secret.token`, so run
it in the folder you serve, or pass `--state-dir`. The server name defaults to
the [server identity](configuration.md#server-identity); use `--name` to choose another.

| Client | Install | What it changes |
|---|---|---|
| Claude Code | `dir2mcp install claude-code [--scope user\|local]` | Calls `claude mcp add-json` with an HTTP entry for the daemon URL. The entry holds no token: its `headersHelper` reads the token file at each connection. The default scope is `user` (all projects); `local` is the current project only. dir2mcp refuses `--scope project`, because that scope writes into `.mcp.json` in your working tree |
| Cursor | `dir2mcp install cursor [--config-path PATH]` | Adds a `url` + `headers` entry to `~/.cursor/mcp.json`. For a project config, pass `--config-path .cursor/mcp.json` and keep that file out of version control |
| Claude Desktop | `dir2mcp install claude [--config-path PATH]` | Adds a `bunx mcp-remote` bridge entry to `claude_desktop_config.json` |

Claude Code and Cursor speak Streamable HTTP to the daemon directly, so they
need no bridge. Claude Code keeps its servers in `~/.claude.json`, a file that
Claude Code itself also rewrites; dir2mcp uses the `claude` CLI and does not
edit that file. The token is not in `~/.claude.json` and not on any command
line, and a new token takes effect at the next connection. When `claude` is not
on `PATH`, `install claude-code` exits non-zero and prints the command to run.
That command also holds no token.

Every file edit is atomic, writes mode `0600` (the entry holds the token), keeps
all other servers and unknown keys, and replaces only the dir2mcp entry, so a
second install is safe. `dir2mcp doctor <client>` checks that the entry
exists, that it uses the current daemon URL, and that the daemon answers. For
Cursor it also checks that the entry holds the current token. `dir2mcp uninstall
<client>` removes only the dir2mcp entry. When the daemon URL or token changes,
run `install` again.

### Recovering from a failed embed run

When the embedding provider rejects a request for a reason that is not the chunk's fault (a key revoked, rotated or billing-suspended mid-run, a quota that went hard-limit, an upstream outage), the affected chunks are recorded as failed and are **not** retried on their own. Restarting the daemon with a working credential does not help by itself: the chunks are in an error state, not a pending one, so `status` reports `embedded_pending=0, errors=N` and the worker sits idle.

Fix the provider first, then re-queue the failed chunks:

```bash
dir2mcp reindex --embeddings-only                          # retry the provider-side failures
dir2mcp reindex --embeddings-only --error-category auth    # or just one category
```

This re-runs only the embed step: extraction (OCR, transcription, media analysis) is **not** repeated, which is the whole point — extraction is usually the expensive half and its output has not changed. It is safe to run while the daemon is up; the running embed worker picks the chunks up on its next cycle. With no daemon running, start one with `dir2mcp up`.

By default it retries the categories a provider fix can plausibly clear: `auth`, `rate_limit`, `transient_net`, and `unknown` (the catch-all for failures the classifier could not label). Failures that are a property of the stored input — `payload_too_large`, `parse_error`, `embedding_failure`, `quality_gate` — are left alone, because re-sending identical bytes to the same provider just fails again; those need a real re-ingest (`dir2mcp reindex`) after changing the input or the configuration. `dir2mcp doctor` names the retryable count when there is one.

### Auto-start at login (macOS)

`dir2mcp up` runs a background daemon, but it does not come back on its own after a reboot. `dir2mcp service install` registers a per-corpus launchd agent that restarts `dir2mcp up --foreground` at every login (and on crash), so the MCP server stays connected across reboots. Use `service status` to inspect it and `service uninstall` to remove it.

The launchd job starts from a clean environment and will **not** inherit a `MISTRAL_API_KEY` you only `export`ed in a shell. Persist the credential first with `dir2mcp config init` (writes `.env.local` in the corpus directory) so the booted daemon can find it; `service install` warns when no persisted credential is present.

`service install` sweeps **every** credential the effective config needs, not just provider API keys: the S3 source credentials (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`), `QDRANT_API_KEY`, `DIR2MCP_INDEX_PGVECTOR_DSN`, `DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL`, and `DIR2MCP_X402_FACILITATOR_TOKEN`. These have no config-file home by design, so the environment / keychain / `.env.local` is their only source. Any of them found in your current shell is copied into `.env.local`; any that is **required** by the config and has no persistent source is named in a warning (and in `missing_credentials` under `--json`) — the service is installed, but it will not boot until you give that secret a persistent source. Values are never printed.

Install and uninstall are also fail-safe: a supervisor step that fails during install rolls the previous service definition (and its loaded/enabled state) back, and `uninstall` refuses to delete a unit whose daemon it could not verifiably stop, so a running daemon is never orphaned. On Linux, `service status` returns an error rather than reporting `installed, not running` when `systemctl --user` cannot answer (no user bus, permission denied, systemctl missing).
