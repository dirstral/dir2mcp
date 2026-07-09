#!/usr/bin/env python3
"""Patch dir2mcp-full.rb's version, release-tarball URLs, and SHA256 fields.

Why this exists
---------------
GoReleaser auto-updates the lean ``dir2mcp.rb`` formula on every release,
but ``dir2mcp-full.rb`` carries hand-written docling install logic
(venv setup, dylib rpath fixes, ruby-macho usage) that GoReleaser cannot
template. Without an automated bump path the full formula drifts every
release; this script keeps it in lockstep by rewriting only the version,
release-tarball URLs, and SHA256 lines, leaving every other line
untouched (``depends_on``, custom install methods, etc).

Homebrew ``revision``
---------------------
On a **real version bump** (the formula's declared version differs from
``--version``) any ``revision N`` line is dropped. ``revision`` is a
Homebrew rebuild counter scoped to a single version; a stale hand-added
``revision`` carried across a version bump yields an installed version
``X.Y.Z_N`` that mismatches the lean ``dir2mcp.rb`` (which GoReleaser
regenerates clean, without a revision). Dropping it keeps the two
formulas in lockstep. When ``--version`` equals the current version (an
idempotent re-run, e.g. a same-version rebuild), any ``revision`` is left
untouched so a deliberate rebuild counter survives.

Usage
-----
    bump_full_formula.py \\
        --version 0.5.0 \\
        --checksums dist/checksums.txt \\
        --formula path/to/Formula/dir2mcp-full.rb

The script is idempotent: re-running it with the same version produces
no further diffs.

The release tarball URLs are matched by the GoReleaser archive
``name_template`` shape (``dir2mcp_<VERSION>_<OS>_<ARCH>.tar.gz``);
non-tarball URLs (``homepage``, references inside install scripts) are
left alone.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

# GoReleaser archive name template:
#   {{ .Binary }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}.tar.gz
TARBALL_RE = re.compile(
    r"dir2mcp_(?P<version>[0-9][0-9A-Za-z.\-+]*)_(?P<os>[a-z]+)_(?P<arch>[a-z0-9_]+)\.tar\.gz"
)
URL_LINE_RE = re.compile(r'^(\s*url\s+)"(?P<url>[^"]+)"\s*$')
SHA256_LINE_RE = re.compile(r'^(\s*sha256\s+)"(?P<hex>[0-9a-fA-F]{64})"\s*$')
VERSION_LINE_RE = re.compile(r'^(\s*version\s+)"(?P<version>[^"]+)"\s*$')
# Homebrew rebuild counter, e.g. ``  revision 1``.
REVISION_LINE_RE = re.compile(r"^\s*revision\s+\d+\s*$")


def _find_declared_version(formula_text: str) -> str | None:
    """Return the formula's current top-level ``version`` string, if any."""
    for line in formula_text.splitlines():
        m = VERSION_LINE_RE.match(line)
        if m:
            return m.group("version")
    return None


def parse_checksums(checksums_path: Path) -> dict[str, str]:
    """Return a mapping ``{filename: sha256}`` from a GoReleaser checksums file.

    GoReleaser writes ``<sha256>  <filename>`` per line.
    """
    checksums: dict[str, str] = {}
    for lineno, raw in enumerate(checksums_path.read_text().splitlines(), start=1):
        line = raw.strip()
        if not line:
            continue
        parts = line.split()
        if len(parts) < 2:
            raise SystemExit(
                f"{checksums_path}:{lineno}: malformed checksum line; "
                f"expected '<sha256>  <filename>', got: {raw!r}"
            )
        sha, filename = parts[0], parts[-1]
        if re.fullmatch(r"[0-9a-fA-F]{64}", sha) is None:
            raise SystemExit(
                f"{checksums_path}:{lineno}: invalid sha256 {sha!r} for {filename!r}; "
                "expected exactly 64 hexadecimal characters"
            )
        checksums[filename] = sha
    return checksums


def bump_formula(formula_text: str, new_version: str, checksums: dict[str, str]) -> str:
    """Return the formula text with version, tarball URLs, and SHA256s rewritten.

    The function walks line by line so that each ``sha256`` line can be
    matched to the URL immediately preceding it (the only structural
    invariant we rely on, and one that the existing formula already
    follows). Lines that do not look like a release-tarball reference are
    passed through untouched.
    """
    out_lines: list[str] = []
    pending_sha_for: str | None = None  # filename whose SHA must update next
    seen_keys: set[str] = set()
    version_line_seen = False

    # A "real version bump" is when the formula's declared version differs from
    # the target. On such a bump we drop any stale `revision N` line (see the
    # module docstring); on an idempotent same-version re-run we leave it alone.
    old_version = _find_declared_version(formula_text)
    is_version_bump = old_version is not None and old_version != new_version

    for line in formula_text.splitlines(keepends=False):
        # Top-level version field — there is exactly one in the formula.
        m_version = VERSION_LINE_RE.match(line)
        if m_version:
            out_lines.append(f'{m_version.group(1)}"{new_version}"')
            version_line_seen = True
            continue

        # Drop a stale rebuild counter when the version actually changes.
        if is_version_bump and REVISION_LINE_RE.match(line):
            continue

        m_url = URL_LINE_RE.match(line)
        if m_url:
            url = m_url.group("url")
            tar_match = TARBALL_RE.search(url)
            if tar_match:
                old_version = tar_match.group("version")
                old_filename = tar_match.group(0)
                new_filename = (
                    f"dir2mcp_{new_version}_{tar_match.group('os')}_{tar_match.group('arch')}.tar.gz"
                )
                # Rewrite both the version path segment and the filename.
                # Validate the substring was actually present so a future
                # url shape change can't silently leave the URL pointing at
                # the old tarball while the SHA below it gets bumped.
                expected_segment = f"/v{old_version}/{old_filename}"
                if expected_segment not in url:
                    raise SystemExit(
                        f"url {url!r} does not contain the expected "
                        f"{expected_segment!r} segment; refusing to rewrite "
                        f"because the SHA256 line below it would otherwise be "
                        f"updated to a mismatched filename"
                    )
                new_url = url.replace(
                    expected_segment,
                    f"/v{new_version}/{new_filename}",
                )
                out_lines.append(f'{m_url.group(1)}"{new_url}"')
                pending_sha_for = new_filename
                seen_keys.add(new_filename)
                continue

        m_sha = SHA256_LINE_RE.match(line)
        if m_sha and pending_sha_for is not None:
            new_sha = checksums.get(pending_sha_for)
            if new_sha is None:
                raise SystemExit(
                    f"checksums.txt has no entry for {pending_sha_for}; "
                    f"available keys: {sorted(checksums)}"
                )
            out_lines.append(f'{m_sha.group(1)}"{new_sha}"')
            pending_sha_for = None
            continue

        out_lines.append(line)

    if pending_sha_for is not None:
        raise SystemExit(
            f"unmatched url for {pending_sha_for}: expected a sha256 line to follow"
        )

    if not seen_keys:
        raise SystemExit("no dir2mcp release-tarball URLs found in formula; nothing to bump")

    if not version_line_seen:
        # If the formula ever loses the top-level version field, just
        # bumping URLs/SHA256s would produce an inconsistent file (the
        # declared version would lag the artefacts). Fail rather than
        # silently ship a mismatch.
        raise SystemExit(
            'no top-level version "..." line found in formula; refusing to '
            "rewrite URLs/SHA256s without also bumping the declared version"
        )

    # Trailing newline preservation.
    suffix = "\n" if formula_text.endswith("\n") else ""
    return "\n".join(out_lines) + suffix


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="new dir2mcp version (e.g. 0.5.0)")
    parser.add_argument(
        "--checksums",
        required=True,
        type=Path,
        help="path to GoReleaser checksums.txt",
    )
    parser.add_argument(
        "--formula",
        required=True,
        type=Path,
        help="path to dir2mcp-full.rb",
    )
    args = parser.parse_args(argv)

    if not args.checksums.is_file():
        raise SystemExit(f"checksums file not found: {args.checksums}")
    if not args.formula.is_file():
        raise SystemExit(f"formula file not found: {args.formula}")

    checksums = parse_checksums(args.checksums)
    if not checksums:
        raise SystemExit(f"no checksum entries parsed from {args.checksums}")

    original = args.formula.read_text()
    bumped = bump_formula(original, args.version, checksums)
    if bumped == original:
        print(f"no changes (formula already at v{args.version})")
        return 0

    args.formula.write_text(bumped)
    print(f"bumped {args.formula} to v{args.version}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
