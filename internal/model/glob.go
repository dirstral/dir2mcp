package model

import (
	"fmt"
	"regexp"
	"strings"
)

// Canonical path-glob matcher shared by the search/ask `file_glob` filter and
// the store's `list_files` glob (issue #441). Before this, `file_glob` used Go
// `path.Match` (segment-aware `*`, no `**`, case-sensitive) while `list_files`
// used SQLite `GLOB` (`*` crosses `/`, no special `**`, case-sensitive) — so the
// SAME pattern returned DIFFERENT file sets and `**` was silently ignored on the
// retrieval side. Both surfaces now compile the pattern here so they can never
// drift, mirroring how NormalizePathPrefix already unified `path_prefix`.
//
// Semantics (one canonical dialect):
//   - `*`  matches any run of characters WITHIN a single path segment (it does
//     NOT cross `/`).
//   - `**` matches across segments: `**/` matches zero or more leading segments
//     (so `**/x` matches `x`, `a/x`, and `a/b/x`); a `**` not followed by `/`
//     matches any characters including `/`.
//   - `?`  matches exactly one character that is not `/`.
//   - `\x` matches the literal character `x` (so a caller can match a path that
//     itself contains a glob metacharacter — this is what escapeGlobLiteral
//     relies on). A trailing backslash is an invalid pattern.
//   - every other byte matches itself, with ASCII letters folded
//     case-insensitively to agree with `path_prefix` (MatchesPathPrefix, which
//     mirrors SQLite LIKE's ASCII-only case folding).
//
// Character classes (`[...]`) are NOT interpreted: `[` and `]` match literally,
// matching the exclude-glob dialect this reuses and keeping escapeGlobLiteral's
// escaping unambiguous.
type CompiledGlob struct {
	re *regexp.Regexp
}

// CompileGlob compiles a path glob into a matcher. It returns an error only for a
// syntactically invalid pattern (currently: a trailing unescaped backslash).
func CompileGlob(pattern string) (*CompiledGlob, error) {
	src, err := globToRegexpSource(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, err
	}
	return &CompiledGlob{re: re}, nil
}

// Match reports whether relPath matches the compiled glob.
func (g *CompiledGlob) Match(relPath string) bool {
	if g == nil || g.re == nil {
		return false
	}
	return g.re.MatchString(relPath)
}

// MatchGlob reports whether relPath matches pattern under the canonical dialect.
// It is a convenience wrapper around CompileGlob for one-shot matches; hot paths
// that reuse a pattern should CompileGlob once and reuse the result.
func MatchGlob(pattern, relPath string) (bool, error) {
	g, err := CompileGlob(pattern)
	if err != nil {
		return false, err
	}
	return g.Match(relPath), nil
}

// globToRegexpSource translates a canonical path glob into an anchored regular
// expression source string.
func globToRegexpSource(glob string) (string, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		c := glob[i]
		switch c {
		case '\\':
			if i+1 >= len(glob) {
				return "", fmt.Errorf("invalid glob %q: trailing backslash", glob)
			}
			writeGlobLiteral(&b, glob[i+1])
			i += 2
			continue
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			writeGlobLiteral(&b, c)
		}
		i++
	}
	b.WriteString("$")
	return b.String(), nil
}

// writeGlobLiteral emits a single literal byte into the regexp source. ASCII
// letters are folded case-insensitively as a two-element class ([Aa]) so matching
// agrees with path_prefix's ASCII-only fold; regexp metacharacters are escaped;
// all other bytes (including UTF-8 continuation bytes) are written verbatim.
func writeGlobLiteral(b *strings.Builder, c byte) {
	switch {
	case c >= 'A' && c <= 'Z':
		b.WriteByte('[')
		b.WriteByte(c)
		b.WriteByte(c + ('a' - 'A'))
		b.WriteByte(']')
	case c >= 'a' && c <= 'z':
		b.WriteByte('[')
		b.WriteByte(c - ('a' - 'A'))
		b.WriteByte(c)
		b.WriteByte(']')
	default:
		if strings.IndexByte(`.+()|[]{}^$\*?`, c) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
}
