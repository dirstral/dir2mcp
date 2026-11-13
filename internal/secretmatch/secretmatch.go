// Package secretmatch holds the one matcher that every secret screen uses
// (SPEC §7.2, spec 0.93.0): the raw-byte scan and the derived-text screen in
// ingest, the open_file reads and the serve-time screen in retrieval, and the
// annotate/transcribe gate in the MCP server.
//
// A Matcher holds two lists. security.secret_patterns finds text that looks
// like a credential. security.secret_allow_patterns finds text that looks like
// a credential but is not one (a redirect parameter, a pagination cursor). The
// matcher finds every match of every secret pattern. An allow pattern can
// cancel only a match of the generic bearer-token default (GenericTokenPattern);
// a match of any other secret pattern (an AWS key, an sk- key, an operator
// pattern) is never cancelled, also when an allow match covers it. A generic
// token match is cancelled when an allow match in the same text covers it: the
// allow match starts at or before the start of the secret match and ends at or
// after its end. The text matches when at least one secret match is not
// cancelled. So an allow pattern releases only the text it covers, and a text
// that holds a redirect link and a real credential still matches.
package secretmatch

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// GenericTokenPattern is the generic bearer-token default of
// security.secret_patterns (SPEC §7.2). It is the only secret pattern whose
// matches an allow pattern can cancel. The comparison is on the exact pattern
// source, so an operator pattern that differs by one character is never
// cancelled.
const GenericTokenPattern = `(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`

// Matcher applies a secret pattern list and an allow pattern list. The zero
// value and a nil *Matcher match nothing. A Matcher is immutable after it is
// built, so one value can serve concurrent callers.
type Matcher struct {
	secret []*regexp.Regexp
	allow  []*regexp.Regexp
}

// Compile builds a Matcher from the configured lists. Blank entries are
// skipped. An entry that does not compile is an error that names the list and
// the entry, so a caller can report it as CONFIG_INVALID.
func Compile(secretPatterns, allowPatterns []string) (*Matcher, error) {
	secret, err := compileList("secret pattern", secretPatterns)
	if err != nil {
		return nil, err
	}
	allow, err := compileList("secret allow pattern", allowPatterns)
	if err != nil {
		return nil, err
	}
	return &Matcher{secret: secret, allow: allow}, nil
}

// New builds a Matcher from compiled lists. Nil entries are skipped.
func New(secret, allow []*regexp.Regexp) *Matcher {
	return &Matcher{secret: nonNil(secret), allow: nonNil(allow)}
}

func compileList(kind string, patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		rx, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile %s %q: %w", kind, pattern, err)
		}
		compiled = append(compiled, rx)
	}
	return compiled, nil
}

func nonNil(in []*regexp.Regexp) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(in))
	for _, rx := range in {
		if rx != nil {
			out = append(out, rx)
		}
	}
	return out
}

// Enabled reports whether the matcher holds at least one secret pattern. With
// none, Match is always false.
func (m *Matcher) Enabled() bool {
	return m != nil && len(m.secret) > 0
}

// SecretSources returns the source strings of the secret patterns, in order.
func (m *Matcher) SecretSources() []string {
	if m == nil {
		return nil
	}
	return sources(m.secret)
}

// AllowSources returns the source strings of the allow patterns, in order.
func (m *Matcher) AllowSources() []string {
	if m == nil {
		return nil
	}
	return sources(m.allow)
}

func sources(list []*regexp.Regexp) []string {
	out := make([]string, 0, len(list))
	for _, rx := range list {
		out = append(out, rx.String())
	}
	return out
}

// MatchString is Match for a string.
func (m *Matcher) MatchString(text string) bool {
	if !m.Enabled() || text == "" {
		return false
	}
	return m.Match([]byte(text))
}

// Match reports whether text holds at least one secret match that no allow
// match covers.
func (m *Matcher) Match(text []byte) bool {
	if !m.Enabled() || len(text) == 0 {
		return false
	}
	var cover *coverIndex
	for _, rx := range m.secret {
		if !m.cancellable(rx) {
			// No allow match can cancel this pattern: one match is enough.
			if rx.Match(text) {
				return true
			}
			continue
		}
		matches := rx.FindAllIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		if cover == nil {
			cover = newCoverIndex(m.allowSpans(text), -1)
		}
		for _, loc := range matches {
			if !cover.covers(loc[0], loc[1]) {
				return true
			}
		}
	}
	return false
}

// cancellable reports whether an allow match can cancel a match of rx: only
// the generic bearer-token default qualifies, and only when an allow list is
// set.
func (m *Matcher) cancellable(rx *regexp.Regexp) bool {
	return len(m.allow) > 0 && rx.String() == GenericTokenPattern
}

// allowSpans returns every match of every allow pattern in text.
func (m *Matcher) allowSpans(text []byte) [][]int {
	var spans [][]int
	for _, rx := range m.allow {
		spans = append(spans, rx.FindAllIndex(text, -1)...)
	}
	return spans
}

// coverIndex answers "does ONE allow span cover [start, end)?" in O(log n).
//
// The spans are sorted by start once. maxEnd[i] is the largest end among the
// spans 0..i, so for a secret match at start, maxEnd at the last span with
// span.start <= start is the end of one single span that starts at or before
// the match. That span covers the match exactly when its end is at or after
// the match end. A running maximum therefore never joins two spans into a
// cover: the value it reports is the end of one span, and every span it looks
// at starts at or before the match.
//
// A span that ends at uncertainEnd (when uncertainEnd >= 0) is kept out of
// maxEnd and recorded in minUncertainStart instead: a streaming caller cannot
// trust it until it has seen the bytes after it (see Stream).
type coverIndex struct {
	starts []int
	maxEnd []int
	// minUncertainStart[i] is the smallest start among the spans 0..i that end
	// at uncertainEnd, or -1.
	minUncertainStart []int
}

func newCoverIndex(spans [][]int, uncertainEnd int) *coverIndex {
	sorted := append([][]int(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	ci := &coverIndex{
		starts:            make([]int, len(sorted)),
		maxEnd:            make([]int, len(sorted)),
		minUncertainStart: make([]int, len(sorted)),
	}
	runEnd, runUncertain := -1, -1
	for i, span := range sorted {
		ci.starts[i] = span[0]
		if uncertainEnd >= 0 && span[1] == uncertainEnd {
			if runUncertain < 0 {
				runUncertain = span[0]
			}
		} else if span[1] > runEnd {
			runEnd = span[1]
		}
		ci.maxEnd[i] = runEnd
		ci.minUncertainStart[i] = runUncertain
	}
	return ci
}

// last returns the index of the last span that starts at or before start, or
// -1 when there is none.
func (ci *coverIndex) last(start int) int {
	return sort.Search(len(ci.starts), func(i int) bool { return ci.starts[i] > start }) - 1
}

// covers reports whether one certain span covers [start, end).
func (ci *coverIndex) covers(start, end int) bool {
	i := ci.last(start)
	return i >= 0 && ci.maxEnd[i] >= end
}

// uncertainCover returns the smallest start of an uncertain span that starts at
// or before start, or -1. An uncertain span ends at the window end, and a
// secret match always ends at or before the window end, so any such span
// covers the match.
func (ci *coverIndex) uncertainCover(start int) int {
	i := ci.last(start)
	if i < 0 {
		return -1
	}
	return ci.minUncertainStart[i]
}

// Redact replaces every secret match in msg with "[REDACTED]". It ignores the
// allow list: a diagnostic message loses nothing it needs when a redirect
// parameter is hidden, and a redaction must not depend on context that a
// truncated message may have lost.
func (m *Matcher) Redact(msg string) string {
	if m == nil || msg == "" {
		return msg
	}
	out := msg
	for _, rx := range m.secret {
		out = rx.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}
