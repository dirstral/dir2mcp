// Package secretmatch holds the one matcher that every secret screen uses
// (SPEC §7.2, spec 0.93.0): the raw-byte scan and the derived-text screen in
// ingest, the open_file reads and the serve-time screen in retrieval, and the
// annotate/transcribe gate in the MCP server.
//
// A Matcher holds two lists. security.secret_patterns finds text that looks
// like a credential. security.secret_allow_patterns finds text that looks like
// a credential but is not one (the redir_token of a YouTube redirect link). The
// matcher finds every match of every secret pattern. An allow pattern can
// cancel only a match of the generic bearer-token default (GenericTokenPattern);
// a match of any other secret pattern (an AWS key, an sk- key, an operator
// pattern) is never cancelled, also when an allow match covers it. A generic
// token match is cancelled when an allow match in the same text covers it: the
// allow match starts at or before the start of the secret match and ends at or
// after its end. The text matches when at least one secret match is not
// cancelled. A generic token match is judged at every start position, so a
// match that starts inside a cancelled match is still judged on its own. So an
// allow pattern releases only the text it covers, and a text that holds a
// redirect link and a real credential still matches.
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
		if !rx.Match(text) {
			continue
		}
		if cover == nil {
			cover = newCoverIndex(m.allowSpans(text))
		}
		if uncoveredAtAnyStart(rx, text, cover) {
			return true
		}
	}
	return false
}

// uncoveredAtAnyStart reports whether rx has a match, at ANY start position in
// text, that no allow span covers (SPEC §7.2, spec 0.93.0). A search for
// non-overlapping matches resumes after a match, so a match that starts inside
// a cancelled match would never be judged: in `redir_token=<20 chars>GITHUB_TOKEN=<20 chars>`
// the first match (`token=<20 chars>GITHUB_TOKEN`) is cancelled, and the second
// assignment starts inside it. So after a cancelled match the search starts
// again one byte after the start of that match, not after its end.
//
// The cost stays linear in practice: each new search scans from the start of
// the last match to the end of the next one, so the extra work per cancelled
// match is bounded by the length of that match, which an allow span covers.
// The generic token pattern has no anchor and no word-boundary assertion, so a
// search in text[pos:] finds exactly the matches that start at or after pos.
func uncoveredAtAnyStart(rx *regexp.Regexp, text []byte, cover *coverIndex) bool {
	for pos := 0; pos < len(text); {
		loc := rx.FindIndex(text[pos:])
		if loc == nil {
			return false
		}
		start, end := pos+loc[0], pos+loc[1]
		if !cover.covers(start, end) {
			return true
		}
		pos = start + 1
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
type coverIndex struct {
	starts []int
	maxEnd []int
}

func newCoverIndex(spans [][]int) *coverIndex {
	sorted := append([][]int(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	ci := &coverIndex{
		starts: make([]int, len(sorted)),
		maxEnd: make([]int, len(sorted)),
	}
	runEnd := -1
	for i, span := range sorted {
		ci.starts[i] = span[0]
		if span[1] > runEnd {
			runEnd = span[1]
		}
		ci.maxEnd[i] = runEnd
	}
	return ci
}

// covers reports whether one allow span covers [start, end).
func (ci *coverIndex) covers(start, end int) bool {
	i := sort.Search(len(ci.starts), func(i int) bool { return ci.starts[i] > start }) - 1
	return i >= 0 && ci.maxEnd[i] >= end
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
