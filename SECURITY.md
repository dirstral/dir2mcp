# Security policy

## Report a vulnerability

Do not open a public issue for a security problem.

Report it privately through GitHub:
[Report a vulnerability](https://github.com/dirstral/dir2mcp/security/advisories/new).
Only the maintainers can read the report.

Put these items in the report:

- the dir2mcp version (`dir2mcp version`)
- the operating system and how you installed dir2mcp
- the steps that show the problem, and what an attacker gets from it

We reply within five working days. We tell you when a fix is ready and credit
you in the release notes, if you want credit.

## Supported versions

We fix security problems in the latest release only. Update to the latest
release before you report.

## Scope

These are in scope:

- the `dir2mcp` binary and its MCP server
- authentication and the `--public` bind guard
- path handling: access outside the served root, or indexing of a file that a
  default `security.path_excludes` pattern must keep out
- x402 request gating
- secrets in logs, support bundles or tool output

A problem in a model provider, an MCP client or a third-party tool is out of
scope. Report it to that project.

## Safe defaults

dir2mcp listens on `127.0.0.1` by default. `--public` refuses to start without
authentication unless you set `--force-insecure`. The default
`security.path_excludes` list keeps credential files (for example `.env`,
`*.pem`, `*.key` and `id_rsa`) out of the index. `dir2mcp support-bundle` redacts
credentials. See [What a support bundle discloses](README.md#what-a-support-bundle-discloses).
