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
	var allowSpans [][]int
	allowDone := false
	for _, rx := range m.secret {
		if len(m.allow) == 0 || rx.String() != GenericTokenPattern {
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
		if !allowDone {
			allowSpans = m.allowSpans(text)
			allowDone = true
		}
		for _, loc := range matches {
			if !covered(allowSpans, loc[0], loc[1]) {
				return true
			}
		}
	}
	return false
}

// allowSpans returns every match of every allow pattern in text.
func (m *Matcher) allowSpans(text []byte) [][]int {
	var spans [][]int
	for _, rx := range m.allow {
		spans = append(spans, rx.FindAllIndex(text, -1)...)
	}
	return spans
}

// covered reports whether one allow span covers [start, end).
func covered(spans [][]int, start, end int) bool {
	for _, span := range spans {
		if span[0] <= start && end <= span[1] {
			return true
		}
	}
	return false
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
