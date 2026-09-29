# Install dir2mcp

This page holds the full install options. The short path is in the
[README](../README.md#try-it-in-two-minutes).

## Installation

Install `dir2mcp` via Homebrew tap:

```bash
brew tap dirstral/tap
brew install dirstral/tap/dir2mcp
```

Install by the full name `dirstral/tap/dir2mcp`. Recent Homebrew does not load formulae from an untrusted third-party tap, but an install by the full name trusts that one formula only ("Trusted formula dirstral/tap/dir2mcp"), so you do not need to trust the whole tap. If your Homebrew still refuses the formula as untrusted, run `brew trust --formula dirstral/tap/dir2mcp` and install again. Older Homebrew has no trust check and installs it directly.

Then verify:

```bash
dir2mcp version
```

### Install tracks: `dir2mcp` vs `dir2mcp-full`

dir2mcp ships in two Homebrew formulas that install the **same binary** but differ in whether the [docling](https://github.com/docling-project/docling) structured-extraction runtime is bundled:

| Track | Install | docling | Footprint | Pick when |
|---|---|---|---|---|
| **Lean** (default) | `brew install dirstral/tap/dir2mcp` | **Not bundled** — bring your own | installs in ~seconds (only `libcap` + `bubblewrap`) | You already have `docling`, run a `docling-serve` container, extract via Mistral OCR, or index docling-free corpora |
| **Full** | `brew install dirstral/tap/dir2mcp-full` | **Bundled** (docling runtime included) | ≈ 6.3 GB installed / ~3 min build | You want local structured PDF/image extraction with zero extra setup |

The two formulas install the **same binary**, so they are mutually exclusive — both provide a `dir2mcp` runtime and Homebrew refuses to have both linked at once. To **switch tracks**, first unlink (or uninstall) the currently-installed one:

```bash
brew unlink dir2mcp        # or: brew uninstall dir2mcp
brew install dirstral/tap/dir2mcp-full
```

**Full footprint:** the full formula bundles a Python 3.12 venv with docling, torch/torchvision, scipy, and shapely, pulling a large dependency chain (llvm, rust, python, openssl, …). Measured at ≈ 6.3 GB installed (~39k files) and ~3 min to build on Linux x86_64 (Homebrew 6.0.6); macOS and prebuilt-bottle installs will differ. The lean formula, by contrast, installs in seconds with only `libcap` + `bubblewrap`.

Choose **full** for batteries-included local extraction; choose **lean** if you bring docling yourself, run docling-serve, or rely on Mistral OCR. Either way, extraction is configurable at runtime via `ingest.extractor` (see [Document extraction](configuration.md#document-extraction-modes--fallback)). To move from lean to full (or to a shared docling-serve) in stages without a re-index flag day, see [Migration & rollout](configuration.md#migration--rollout-adopting-docling-in-stages).

### Docker

Each release publishes a multi-arch image (linux/amd64, linux/arm64):

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD:/corpus:ro" -v dir2mcp-state:/state \
  ghcr.io/dirstral/dir2mcp:latest
```

The container serves `http://localhost:8080/mcp` with bearer-token auth. Read
the token with `docker run --rm -v dir2mcp-state:/state alpine cat /state/secret.token`.

Models: a `.dir2mcp.yaml` in the mounted folder is used when there is one.
Otherwise the image default binds embeddings and answers to an Ollama on the
Docker host at `host.docker.internal:11434` (Docker Desktop and Colima; on
Linux add `--add-host=host.docker.internal:host-gateway`). The server starts
and serves its tools when that Ollama is not reachable, and indexing waits
for it. For a cloud provider, put the key in the environment (`-e MISTRAL_API_KEY=...`)
and mount a `.dir2mcp.yaml` that binds to it.

The image holds ffmpeg for audio and video, and no docling: for PDFs and
images use `ingest.extractor: mistral` or a docling-serve container
([document extraction](configuration.md#docling-extraction-over-http-docling-serve)).
`docker build -t dir2mcp .` builds the same image from source.

### Nix (macOS + Linux)

A [Nix flake](../flake.nix) packages the **lean** `dir2mcp` binary for `x86_64`/`aarch64` on both Linux and macOS. Run it without installing:

```bash
nix run github:dirstral/dir2mcp -- version
```

Or add it to a profile:

```bash
nix profile install github:dirstral/dir2mcp
```

The flake builds the lean binary only (no bundled docling runtime); for batteries-included structured extraction, use the `dir2mcp-full` Homebrew formula or the docling-full container. A `devShells.default` with the Go toolchain is also exposed (`nix develop`). The flake also exposes `overlays.default` (adds `pkgs.dir2mcp` to any nixpkgs), plus `darwinModules.default` and `homeManagerModules.default` for the declarative service below.

#### Declarative service (nix-darwin / home-manager)

The flake ships service modules that run `dir2mcp up --foreground` under a supervisor (launchd on macOS, systemd `--user` on Linux), so the corpus server starts at login and is restarted on failure. These require the flake's modules — they are not part of nixpkgs.

Add the input and the module to your config. **nix-darwin:**

```nix
{
  inputs.dir2mcp.url = "github:dirstral/dir2mcp";

  # in your darwinConfiguration modules list:
  modules = [
    dir2mcp.darwinModules.default
    {
      services.dir2mcp = {
        enable = true;
        rootDir = "/Users/me/Documents/corpus";
        # Secrets (MISTRAL_API_KEY, OPENAI_API_KEY, DIR2MCP_AUTH_TOKEN, ...)
        # live in this file, NOT in the nix store. Manage it yourself with
        # restrictive permissions.
        environmentFile = "/Users/me/.config/dir2mcp/env";
      };
    }
  ];
}
```

**home-manager** (works on macOS via launchd and on Linux via systemd `--user`):

```nix
{
  inputs.dir2mcp.url = "github:dirstral/dir2mcp";

  # in your homeConfiguration modules list:
  modules = [
    dir2mcp.homeManagerModules.default
    {
      services.dir2mcp = {
        enable = true;
        rootDir = "/home/me/corpus";
        environmentFile = "/home/me/.config/dir2mcp/env";
      };
    }
  ];
}
```

Optional knobs: `stateDir`, `listen`, `extraArgs` (e.g. `[ "--public" "--auth" "auto" ]`), and `package` (defaults to this flake's lean build). On macOS, launchd has no native `EnvironmentFile`, so the module sources `environmentFile` via a small wrapper script at start; on Linux it is wired to systemd's native `EnvironmentFile=`. Either way the secret values stay outside the world-readable nix store.

### Windows

Each release has a Windows zip for amd64 and arm64 on the
[Releases](https://github.com/dirstral/dir2mcp/releases) page. There is no Scoop
or winget package yet.

1. Download `dir2mcp_<version>_windows_amd64.zip` (or `_windows_arm64.zip`).
2. Extract `dir2mcp.exe` into a folder on your `PATH`.
3. Open a new terminal and run `dir2mcp version`.

Quickstart in PowerShell (fully local, with [Ollama](https://ollama.com)):

```powershell
ollama pull nomic-embed-text; ollama pull qwen2.5:7b
cd $HOME\notes                      # any folder you want to ask about
@'
providers:
  local:
    kind: openai
    base_url: http://127.0.0.1:11434/v1
    embed_text_model: nomic-embed-text
    embed_code_model: nomic-embed-text
    chat_model: qwen2.5:7b
model:
  embed: {provider: local}
  chat: {provider: local}
'@ | Set-Content -Encoding utf8 .dir2mcp.yaml
dir2mcp up                          # the server stays in this terminal
```

With a cloud key instead, set it for the session and skip the file:
`$env:MISTRAL_API_KEY = "..."`, then `dir2mcp up`.

`up` keeps this terminal. Open a second terminal in the same folder to ask:

```powershell
cd $HOME\notes
dir2mcp ask "When is the budget meeting?"
dir2mcp down                        # stops the server in the first terminal
```

What CI proves on Windows: the `windows` job in `.github/workflows/go.yml` runs
the Go test suite on `windows-latest` (amd64). One end-to-end test in that suite
runs `up --foreground` on a folder with nested directories, then `status`,
`list-files`, `ask`, `open-file`, `install claude` and `down`. `ask` returns an
answer with citations. Citations and `list-files` use forward-slash paths such
as `docs/sub/policy.md`, the same as on macOS and Linux.

Limits on Windows:

- `up` stays in the foreground. Windows has no background (daemon) mode, and
  `up --daemon` fails with an error. Keep the terminal open, or stop the server
  with `dir2mcp down` from a second terminal.
- `down` ends the server at once through TerminateProcess. Windows has no
  SIGTERM, so there is no graceful shutdown. The sqlite store uses
  transactions, so the index stays consistent, and the next `up` continues from
  it.
- `dir2mcp service` is not available. To start the server at logon, add a Task
  Scheduler task that runs `dir2mcp up --foreground` in the corpus folder.
- `install claude-code` and `print-config claude-code` are refused: the auth
  helper they register is a POSIX shell command, and it is not tested with
  Claude Code on Windows. Use `install cursor`, or run dir2mcp on macOS or Linux.
- `install claude` writes `%APPDATA%\Claude\claude_desktop_config.json`. The
  entry runs `mcp-remote` through `bunx` or `npx`. We did not test this entry
  with Claude Desktop on Windows.
- `dir2mcp-full` (bundled docling) is a Homebrew formula only. On Windows,
  install docling yourself, use a docling-serve container, or use Mistral OCR.
- A managed recognition backend (`recognize.serve_command`) needs a POSIX `sh`.
  On Windows, start the backend yourself and set only `recognize.serve_url`.
- Owner-only file modes (0600) do not apply on Windows. Files in `.dir2mcp`
  get the access rules of their parent folder, so keep the corpus in your user
  profile.
- The arm64 zip is built, but CI does not test it: the CI job has no Windows
  arm64 runner.

Build-from-source remains available as an alternative:

```bash
git clone --recurse-submodules https://github.com/Dirstral/dir2mcp
cd dir2mcp
make build
```

> **Existing clones:** run `git submodule update --init --recursive` to fetch the `dirstral-spec` submodule.
> To update the spec to the latest pinned version: `git submodule update --remote dirstral-spec`.

## Runtime Prerequisites (By Scenario)

Pick the row that matches how you run `dir2mcp`:

| Scenario | Required |
|---|---|
| Local MCP only (`127.0.0.1`) | `dir2mcp` binary, plus either `docling` or `MISTRAL_API_KEY` depending on extractor/generation mode |
| Public MCP (no tunnel) | Local MCP requirements + reachable host/port + secure auth token mode |
| Public MCP via Cloudflare Tunnel | Local MCP requirements + `cloudflared` installed |
| Public MCP via ngrok | Local MCP requirements + `ngrok` installed + verified ngrok account + authtoken |
| x402-gated MCP | Public MCP requirements + facilitator URL + facilitator token + full x402 route policy fields |

## Build from source

The [two-minute path](../README.md#try-it-in-two-minutes) uses the Homebrew build. To build
it yourself you need Go 1.25.13 or later ([go.dev/dl](https://go.dev/dl/)) and `make`.
An older Go 1.25 downloads the right toolchain by itself, unless you set
`GOTOOLCHAIN=local`.

```bash
git clone https://github.com/Dirstral/dir2mcp
cd dir2mcp
cp .env.example .env        # optional: provider keys (or use a local .dir2mcp.yaml)
# optional: create `.env.local` for local overrides
# (it takes precedence over `.env`)
# cp .env.example .env.local
make build
./dir2mcp up
```

Or build each binary directly:

- `go build -o dir2mcp ./cmd/dir2mcp/`
- `go build -o elevenlabs-bridge ./cmd/elevenlabs-bridge/`

The server prints its MCP endpoint URL on startup. Point your MCP client at that URL.
Non-empty shell environment variables and OS keychain entries take precedence over dotenv files. Among the dotenv files the first non-empty value wins, in the order given under [Where dotenv files are read from](configuration.md#where-dotenv-files-are-read-from).

### Local development environment

`dir2mcp` automatically loads `.env` and `.env.local` from two directories: the directory that holds the resolved config file (the `--config` path, or `.dir2mcp.yaml` in the working directory), then the working directory itself. See [Where dotenv files are read from](configuration.md#where-dotenv-files-are-read-from) for the full order. A **non-empty** shell environment variable takes ultimate precedence; a variable exported as an empty string is treated as unset, so a dotenv file still fills it.
