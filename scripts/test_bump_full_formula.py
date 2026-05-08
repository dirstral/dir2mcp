"""Tests for ``bump_full_formula.bump_formula``.

Run via ``make test-release-tools`` (preferred), or directly:

    cd scripts && python3 -m unittest test_bump_full_formula

The ``cd`` is required because this file imports ``bump_full_formula``
as a sibling module, not as a package — ``scripts/`` does not have an
``__init__.py``. Adding one is intentionally avoided so the patcher
script stays runnable as a plain executable from the release workflow.
"""

from __future__ import annotations

import textwrap
import unittest

from bump_full_formula import bump_formula

_OLD_FORMULA = textwrap.dedent(
    """\
    class Dir2mcpFull < Formula
      desc "Deploy local directories as an MCP server with bundled Docling runtime"
      homepage "https://github.com/dirstral/dir2mcp"
      version "0.4.4"
      license "MIT"
      revision 1

      on_macos do
        if Hardware::CPU.intel?
          url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_darwin_amd64.tar.gz"
          sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        end
        if Hardware::CPU.arm?
          url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_darwin_arm64.tar.gz"
          sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
        end
      end

      on_linux do
        if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?
          url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_linux_amd64.tar.gz"
          sha256 "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
        end
        if Hardware::CPU.arm? && Hardware::CPU.is_64_bit?
          url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_linux_arm64.tar.gz"
          sha256 "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
        end
      end

      def install_docling_runtime
        # Hand-written install logic that must be preserved verbatim.
        ENV["FOO"] = "bar"
      end
    end
    """
)

_NEW_CHECKSUMS = {
    "dir2mcp_0.5.0_darwin_amd64.tar.gz": "1" * 64,
    "dir2mcp_0.5.0_darwin_arm64.tar.gz": "2" * 64,
    "dir2mcp_0.5.0_linux_amd64.tar.gz":  "3" * 64,
    "dir2mcp_0.5.0_linux_arm64.tar.gz":  "4" * 64,
}


class BumpFormulaTests(unittest.TestCase):
    def test_rewrites_version_urls_and_shas(self) -> None:
        out = bump_formula(_OLD_FORMULA, "0.5.0", _NEW_CHECKSUMS)
        self.assertIn('version "0.5.0"', out)
        for filename, sha in _NEW_CHECKSUMS.items():
            self.assertIn(filename, out)
            self.assertIn(sha, out)
        # Old version artefacts must be gone.
        self.assertNotIn("0.4.4", out)
        for old_sha in ("aaaa", "bbbb", "cccc", "dddd"):
            self.assertNotIn(old_sha * 16, out)

    def test_preserves_unrelated_lines(self) -> None:
        out = bump_formula(_OLD_FORMULA, "0.5.0", _NEW_CHECKSUMS)
        # Hand-written install logic, license, revision, homepage all survive.
        self.assertIn("revision 1", out)
        self.assertIn('license "MIT"', out)
        self.assertIn('homepage "https://github.com/dirstral/dir2mcp"', out)
        self.assertIn("def install_docling_runtime", out)
        self.assertIn('ENV["FOO"] = "bar"', out)

    def test_idempotent(self) -> None:
        once = bump_formula(_OLD_FORMULA, "0.5.0", _NEW_CHECKSUMS)
        twice = bump_formula(once, "0.5.0", _NEW_CHECKSUMS)
        self.assertEqual(once, twice)

    def test_missing_checksum_is_fatal(self) -> None:
        partial = dict(_NEW_CHECKSUMS)
        del partial["dir2mcp_0.5.0_linux_arm64.tar.gz"]
        with self.assertRaises(SystemExit) as cm:
            bump_formula(_OLD_FORMULA, "0.5.0", partial)
        self.assertIn("dir2mcp_0.5.0_linux_arm64.tar.gz", str(cm.exception))

    def test_no_release_urls_is_fatal(self) -> None:
        formula = textwrap.dedent(
            """\
            class Dir2mcpFull < Formula
              homepage "https://example.com"
              version "0.4.4"
            end
            """
        )
        with self.assertRaises(SystemExit) as cm:
            bump_formula(formula, "0.5.0", _NEW_CHECKSUMS)
        self.assertIn("no dir2mcp release-tarball URLs", str(cm.exception))

    def test_missing_version_line_is_fatal(self) -> None:
        # The patcher must not silently rewrite URLs/SHA256s while leaving
        # the declared version stale; that would ship a mismatched formula.
        formula = textwrap.dedent(
            """\
            class Dir2mcpFull < Formula
              homepage "https://github.com/dirstral/dir2mcp"
              # version line removed on purpose
              url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_darwin_arm64.tar.gz"
              sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
            end
            """
        )
        with self.assertRaises(SystemExit) as cm:
            bump_formula(formula, "0.5.0", _NEW_CHECKSUMS)
        self.assertIn("no top-level version", str(cm.exception))

    def test_url_without_expected_segment_is_fatal(self) -> None:
        # Tarball name matches our regex but the URL path doesn't contain
        # the /v<version>/<filename> segment we'd rewrite. Must fail rather
        # than leave the URL stale while updating the SHA below it.
        formula = textwrap.dedent(
            """\
            class Dir2mcpFull < Formula
              version "0.4.4"
              url "https://example.com/odd-layout/dir2mcp_0.4.4_darwin_arm64.tar.gz"
              sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
            end
            """
        )
        with self.assertRaises(SystemExit) as cm:
            bump_formula(formula, "0.5.0", _NEW_CHECKSUMS)
        self.assertIn("expected", str(cm.exception))
        self.assertIn("segment", str(cm.exception))

    def test_does_not_touch_non_tarball_urls(self) -> None:
        formula = textwrap.dedent(
            """\
            class Dir2mcpFull < Formula
              homepage "https://github.com/dirstral/dir2mcp"
              version "0.4.4"
              url "https://github.com/dirstral/dir2mcp/releases/download/v0.4.4/dir2mcp_0.4.4_darwin_arm64.tar.gz"
              sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
              # A wholly unrelated url that happens to live in this file.
              # Must NOT be rewritten.
              url "https://example.com/some-other-thing.tar.gz"
            end
            """
        )
        out = bump_formula(formula, "0.5.0", _NEW_CHECKSUMS)
        self.assertIn("https://example.com/some-other-thing.tar.gz", out)


if __name__ == "__main__":
    unittest.main()
