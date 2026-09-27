# Contributing to dir2mcp

Thank you for your help. This page tells you how to report a problem and how
to send a change.

## Report a bug

Open an issue with the bug report form. Attach a support bundle:

```bash
dir2mcp support-bundle
```

The bundle redacts credentials. It also redacts local paths and endpoints
unless you add `--include-content`. Read
[What a support bundle discloses](docs/security.md#what-a-support-bundle-discloses)
before you attach it.

For a security problem, do not open an issue. Follow [SECURITY.md](SECURITY.md).

## Set up

You need Go (the version in `go.mod`), `golangci-lint`, `gocyclo` v0.6.0 and
Python 3 for the annotator suite.

```bash
git clone --recurse-submodules https://github.com/dirstral/dir2mcp.git
cd dir2mcp
make build
```

The `dirstral-spec` directory is a git submodule. If you cloned without
`--recurse-submodules`, run `git submodule update --init`.

## Check a change

```bash
make check   # the full local gate: Go checks plus the Python annotator suite
make fmt     # format the tree in place (the gate only reports, it never rewrites)
```

`make check` must pass before you open a pull request. Put new tests under
`tests/`, in the directory for the subsystem.

## More make targets

```bash
make cyclo                    # the gocyclo -over 15 gate that CI runs (gocyclo v0.6.0)
make lint                     # golangci-lint; part of make check
make benchmark                # the large-corpus retrieval benchmark
make bench-e2e                # the end-to-end answer and citation benchmark (bench/README.md)
make build-elevenlabs-bridge  # the ElevenLabs bridge wrapper binary
make demo                     # record assets/demo.gif again (needs vhs and a local Ollama)
```

## Release and API notes

- A pushed `v*` tag runs `.github/workflows/release.yml`, which publishes the
  release with GoReleaser. The Homebrew formula update needs
  `HOMEBREW_TAP_GITHUB_TOKEN` with write access to `dirstral/homebrew-tap`.
- `retrieval.NewEngine` takes a context as its first parameter:
  `retrieval.NewEngine(ctx, stateDir, rootDir, cfg)`.
- `Engine.Ask` has a context-aware variant, `AskWithContext`. The original
  `Ask` stays as a thin wrapper for compatibility.

## Behaviour changes are spec-first

The contract for tools, errors and defaults lives in
[dirstral-spec](https://github.com/dirstral/dirstral-spec), vendored at
`dirstral-spec/`. A change to observable behaviour lands there first. The
dir2mcp pull request then moves the submodule to the merged spec commit. A bug
fix that makes the code match the spec needs no spec change.

## Pull requests

- Keep a pull request to one issue. Do not include unrelated files.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
  messages and the pull request title.
- If behaviour changes, update the tests and the docs in the same pull request.
- Do not log secrets or raw sensitive payloads.

## License

By contributing, you agree that your contribution is licensed under the
[MIT License](LICENSE).
