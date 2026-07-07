package model

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

// globCache memoizes CompileGlob so a pattern reused across many candidate hits
// in a query hot path (matchFilters / Filter.Match, issue #441) is compiled ONCE
// rather than per candidate — regexp.Compile per hit is a real regression on a
// widened candidate pool. Glob patterns are low-cardinality (operator config +
// per-query filters); growth is bounded by globCacheMax so an unbounded stream of
// distinct patterns can't grow the cache without limit (past the cap, compilation
// still works, just uncached). Entries are immutable, so the sync.Map is safe.
var (
	globCache    sync.Map // pattern string -> globCacheEntry
	globCacheLen atomic.Int64
)

const globCacheMax = 1024

type globCacheEntry struct {
	g   *CompiledGlob
	err error
}

func compileGlobCached(pattern string) (*CompiledGlob, error) {
	if v, ok := globCache.Load(pattern); ok {
		e := v.(globCacheEntry)
		return e.g, e.err
	}
	g, err := CompileGlob(pattern)
	if globCacheLen.Load() < globCacheMax {
		if _, loaded := globCache.LoadOrStore(pattern, globCacheEntry{g: g, err: err}); !loaded {
			globCacheLen.Add(1)
		}
	}
	return g, err
}

// MatchGlob reports whether relPath matches pattern under the canonical dialect.
// Compilation is memoized (globCache), so calling it per candidate hit in a
// filter hot path compiles the pattern only once. A caller with a single pattern
// and many paths may still CompileGlob once and reuse the *CompiledGlob to skip
// even the cache lookup.
func MatchGlob(pattern, relPath string) (bool, error) {
	g, err := compileGlobCached(pattern)
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
