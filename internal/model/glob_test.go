package model

import "testing"

// TestMatchGlob_CanonicalSemantics pins the one canonical path-glob dialect that
// both the search/ask file_glob filter and the list_files glob now share
// (issue #441): segment-aware `*`, recursive `**`, `?` = one non-slash char,
// backslash escaping, and ASCII-case-insensitive literals.
func TestMatchGlob_CanonicalSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// `*` is segment-aware: it does NOT cross `/`.
		{"*.pdf", "a.pdf", true},
		{"*.pdf", "docs/a.pdf", false},
		{"*.pdf", "docs/sub/a.pdf", false},
		{"docs/*", "docs/a.pdf", true},
		{"docs/*", "docs/sub/a.pdf", false},
		{"docs/*.pdf", "docs/a.pdf", true},
		{"docs/*.pdf", "docs/sub/a.pdf", false},

		// `**` crosses segments.
		{"**/*.pdf", "a.pdf", true},
		{"**/*.pdf", "docs/a.pdf", true},
		{"**/*.pdf", "docs/sub/a.pdf", true},
		{"**", "a/b/c.pdf", true},
		{"docs/**", "docs/sub/a.pdf", true},
		{"docs/**/*.pdf", "docs/a.pdf", true},
		{"docs/**/*.pdf", "docs/sub/deep/a.pdf", true},

		// `?` matches exactly one non-slash char.
		{"a?.txt", "ab.txt", true},
		{"a?.txt", "a.txt", false},
		{"a?.txt", "a/b.txt", false},

		// ASCII case-insensitive (agrees with path_prefix).
		{"DOCS/*", "docs/a.pdf", true},
		{"docs/*", "DOCS/A.PDF", true},

		// Backslash escapes a metacharacter to a literal (escapeGlobLiteral relies
		// on this for open_file's exact-path match).
		{`a\*b.txt`, "a*b.txt", true},
		{`a\*b.txt`, "axb.txt", false},
		{`file\?.txt`, "file?.txt", true},
		{`file\?.txt`, "fileX.txt", false},
	}
	for _, c := range cases {
		got, err := MatchGlob(c.pattern, c.path)
		if err != nil {
			t.Errorf("MatchGlob(%q,%q) unexpected error: %v", c.pattern, c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("MatchGlob(%q,%q)=%v want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// TestMatchGlob_TrailingBackslashInvalid pins that a trailing unescaped backslash
// is reported as an invalid pattern.
func TestMatchGlob_TrailingBackslashInvalid(t *testing.T) {
	if _, err := MatchGlob(`docs\`, "docs"); err == nil {
		t.Fatalf("expected error for trailing backslash pattern")
	}
}

// TestFilterPathGlob_MatchesMatchGlob pins that the Filter.Match PathGlob
// predicate (search/ask pushdown path) and the standalone MatchGlob used by
// list_files agree on the same file set for representative patterns — the core
// contract of issue #441 (file_glob and list_files must not diverge).
func TestFilterPathGlob_MatchesMatchGlob(t *testing.T) {
	patterns := []string{"*.pdf", "docs/*", "**/*.pdf", "docs/**", "DOCS/*"}
	paths := []string{
		"a.pdf", "b.txt", "docs/a.pdf", "docs/sub/a.pdf",
		"docs/sub/deep/a.pdf", "src/main.go",
	}
	for _, pat := range patterns {
		for _, p := range paths {
			viaMatchGlob, err := MatchGlob(pat, p)
			if err != nil {
				t.Fatalf("MatchGlob(%q,%q): %v", pat, p, err)
			}
			viaFilter := Filter{PathGlob: pat}.Match(IndexPayload{RelPath: p})
			if viaMatchGlob != viaFilter {
				t.Errorf("divergence for pattern %q path %q: MatchGlob=%v Filter.Match=%v",
					pat, p, viaMatchGlob, viaFilter)
			}
		}
	}
}
