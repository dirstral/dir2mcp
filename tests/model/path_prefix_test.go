package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestNormalizePathPrefix pins the shared path_prefix normalizer (issue #286).
// It is the single source of truth for both list_files (store LIKE query) and
// search/ask filtering, so its behavior must be explicit and stable.
func TestNormalizePathPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{".", ""},
		{"./", ""},
		{"/", ""},
		{"acts", "acts"},
		{"acts/", "acts"},
		{"./acts", "acts"},
		{"/acts", "acts"},
		{"  acts/  ", "acts"},
		{"acts//foo", "acts/foo"},
		{"acts/./foo", "acts/foo"},
		{"acts/foo.pdf", "acts/foo.pdf"},
		{"ACTS", "ACTS"}, // case preserved by the normalizer (folding happens at match time)
		{`acts\foo`, "acts/foo"},
	}
	for _, tc := range cases {
		if got := model.NormalizePathPrefix(tc.in); got != tc.want {
			t.Errorf("NormalizePathPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMatchesPathPrefix pins the shared matcher used by search/ask. It mirrors
// the store's LIKE 'prefix%' semantics: case-insensitive (ASCII) prefix match,
// matching across sub-segments, with an empty/normalized-away prefix matching
// everything.
func TestMatchesPathPrefix(t *testing.T) {
	cases := []struct {
		rel    string
		prefix string
		want   bool
	}{
		{"acts/foo.pdf", "", true},
		{"acts/foo.pdf", "  ", true},
		{"acts/foo.pdf", "acts", true},
		{"acts/foo.pdf", "acts/", true},
		{"acts/foo.pdf", "./acts", true},
		{"acts/foo.pdf", "/acts", true},
		{"acts/foo.pdf", "ACTS", true}, // ASCII case-insensitive, like SQLite LIKE
		{"acts/foo.pdf", "act", true},  // store LIKE matches sub-segments
		{"acts/foo.pdf", "acts/foo.pdf", true},
		{"acts/foo.pdf", "xyz", false},
		{"acts/foo.pdf", "other/", false},
		{"acts/foo.pdf", "acts/foo.pdf/extra", false},
	}
	for _, tc := range cases {
		if got := model.MatchesPathPrefix(tc.rel, tc.prefix); got != tc.want {
			t.Errorf("MatchesPathPrefix(%q, %q) = %v, want %v", tc.rel, tc.prefix, got, tc.want)
		}
	}
}
