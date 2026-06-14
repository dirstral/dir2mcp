package model

import (
	"path"
	"strings"
)

// NormalizePathPrefix canonicalizes a caller-supplied path_prefix into the form
// used to match against slash-normalized rel_paths. It is the single source of
// truth shared by the store's list_files LIKE-prefix query and retrieval's
// search/ask filtering, so the two can never drift (issue #286 Bug B).
//
// rel_paths are stored slash-normalized with no leading "./" or "/" and with
// case preserved. The normalizer therefore:
//   - trims surrounding whitespace
//   - converts backslashes to forward slashes (so a Windows-style prefix maps
//     onto the slash-normalized rel_path domain)
//   - strips a single leading "./"
//   - path.Clean's the result (collapsing "." and redundant separators)
//   - strips a leading "/"
//   - maps a prefix that cleans to "." (i.e. "no prefix") to the empty string
//
// An empty result means "no prefix filter".
func NormalizePathPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	trimmed = strings.TrimPrefix(trimmed, "./")
	trimmed = path.Clean(trimmed)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "." {
		return ""
	}
	return trimmed
}

// MatchesPathPrefix reports whether relPath satisfies the given (raw, not yet
// normalized) path_prefix. It normalizes the prefix via NormalizePathPrefix and
// then applies the same matching rule the store's LIKE 'prefix%' query uses:
// a case-insensitive (ASCII) prefix test. An empty/normalized-away prefix
// matches everything. This mirrors list_files so a prefix that lists a file in
// list_files also matches it in search/ask (issue #286 Bug B).
//
// Note this intentionally matches sub-segments the way SQLite LIKE does: the
// prefix "act" matches "acts/foo.pdf". Keeping the two call sites byte-for-byte
// consistent is the contract; changing segment semantics would change
// list_files behavior too and is out of scope.
func MatchesPathPrefix(relPath, prefix string) bool {
	normalized := NormalizePathPrefix(prefix)
	if normalized == "" {
		return true
	}
	return hasASCIIPrefixFold(relPath, normalized)
}

// hasASCIIPrefixFold reports whether s begins with prefix, comparing ASCII
// letters case-insensitively. This matches SQLite's default LIKE behavior,
// which folds case for ASCII characters only (non-ASCII bytes compare exactly).
func hasASCIIPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if asciiLower(s[i]) != asciiLower(prefix[i]) {
			return false
		}
	}
	return true
}

func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
