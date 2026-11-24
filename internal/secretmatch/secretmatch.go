// Package secretmatch holds the one matcher that every secret screen uses
// (SPEC §7.2, spec 0.99.0): the raw-byte scan and the derived-text screen in
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
// token match is cancelled when the cover of an allow match in the same text
// covers it: the cover starts at or before the start of the secret match and
// ends at or after its end. The cover is the first capture group of the allow
// pattern when it has one (the rest of the match is required context), and the
// whole match when it has none. The text matches when at least one secret match is not
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
	"unicode/utf8"
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
	allow  []allowPattern
}

// allowPattern is one allow pattern and the form the cover search uses
// to find a match at or after a given position, with the text before that
// position as context (see eachMatch).
type allowPattern struct {
	rx *regexp.Regexp
	// after finds the leftmost match that starts after the first rune of the
	// text it searches: `(?s:.)(?:P)`. A search of text[q-n:], where n is the
	// size of the rune before q, finds the leftmost match that starts at or
	// after q, and the assertions of P (^, \b) see the rune before q.
	after *regexp.Regexp
}

func newAllowPattern(rx *regexp.Regexp) allowPattern {
	return allowPattern{rx: rx, after: regexp.MustCompile(`(?s:.)(?:` + rx.String() + `)`)}
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
	return New(secret, allow), nil
}

// New builds a Matcher from compiled lists. Nil entries are skipped.
func New(secret, allow []*regexp.Regexp) *Matcher {
	compiled := nonNil(allow)
	patterns := make([]allowPattern, 0, len(compiled))
	for _, rx := range compiled {
		patterns = append(patterns, newAllowPattern(rx))
	}
	return &Matcher{secret: nonNil(secret), allow: patterns}
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
	out := make([]string, 0, len(m.allow))
	for _, ap := range m.allow {
		out = append(out, ap.rx.String())
	}
	return out
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
	for _, rx := range m.secret {
		if !rx.Match(text) {
			continue
		}
		if !m.cancellable(rx) {
			// No allow match can cancel this pattern: one match is enough.
			return true
		}
		if m.anyUncovered(genericMatches(rx, text), text) {
			return true
		}
	}
	return false
}

// genericMatches returns the match of rx at every start position in text, in
// order of start (SPEC §7.2, spec 0.99.0). A search for non-overlapping
// matches resumes after a match, so a match that starts inside another match
// would never be judged: in `redir_token=<20 chars>GITHUB_TOKEN=<20 chars>`
// the first match (`token=<20 chars>GITHUB_TOKEN`) is cancelled, and the
// second assignment starts inside it. So the search starts again one byte
// after the start of each match, not after its end.
//
// The generic token pattern has no anchor and no word-boundary assertion, so
// a search in text[pos:] finds exactly the matches that start at or after pos.
// Each match starts with the literal `token`, so the list holds at most one
// span for each occurrence of that word.
func genericMatches(rx *regexp.Regexp, text []byte) [][2]int {
	var spans [][2]int
	for pos := 0; pos < len(text); {
		loc := rx.FindIndex(text[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		spans = append(spans, [2]int{start, pos + loc[1]})
		pos = start + 1
	}
	return spans
}

// cancellable reports whether an allow match can cancel a match of rx: only
// the generic bearer-token default qualifies, and only when an allow list is
// set.
func (m *Matcher) cancellable(rx *regexp.Regexp) bool {
	return len(m.allow) > 0 && rx.String() == GenericTokenPattern
}

// anyUncovered reports whether one of the generic matches (sorted by start)
// has no allow cover that holds it whole (SPEC §7.2, spec 0.99.0). The cover
// of an allow match is the text of the first capture group of the pattern
// when it has one, and the whole match when it has none. The rest of the
// match is context: it must be present, but it cancels nothing. A match whose
// first group took no part in it has no cover. One cover must hold the whole
// generic match; two covers are never joined.
//
// The allow matches are read one at a time and are not kept, so the memory
// stays bounded by the number of generic matches, also for a dense allow rule
// on a large text. The search stops when every generic match is cancelled, or
// when the next allow match starts after the last generic match that is still
// uncancelled (a cover never starts before its match, so no later match can
// cancel it).
func (m *Matcher) anyUncovered(generic [][2]int, text []byte) bool {
	if len(generic) == 0 {
		return false
	}
	cancelled := make([]bool, len(generic))
	last := len(generic) - 1
	for _, ap := range m.allow {
		ap.eachMatch(text, func(start int) bool { return start <= generic[last][0] }, func(loc []int) bool {
			cover, ok := coverOf(loc)
			if ok {
				cancelCovered(generic, cancelled, cover)
			}
			for last >= 0 && cancelled[last] {
				last--
			}
			return last >= 0
		})
		if last < 0 {
			return false
		}
	}
	return true
}

// coverOf returns the cover of one allow match: its first capture group when
// the pattern has one, and the whole match when it has none. ok is false when
// the first group took no part in the match.
func coverOf(loc []int) ([2]int, bool) {
	if len(loc) < 4 {
		return [2]int{loc[0], loc[1]}, true
	}
	if loc[2] < 0 || loc[3] < 0 {
		return [2]int{}, false
	}
	return [2]int{loc[2], loc[3]}, true
}

// cancelCovered marks each generic match that cover holds whole.
func cancelCovered(generic [][2]int, cancelled []bool, cover [2]int) {
	i := sort.Search(len(generic), func(i int) bool { return generic[i][0] >= cover[0] })
	for ; i < len(generic) && generic[i][0] <= cover[1]; i++ {
		if generic[i][1] <= cover[1] {
			cancelled[i] = true
		}
	}
}

// eachMatch calls visit with the submatch indexes of the match of the allow
// pattern at every start position in text, in order of start, while want
// accepts the start and visit returns true. Two matches can share context: in
// `;token=<20 chars>;token=<20 chars>;` the pattern `;(token=A{20});` matches
// at the first `;` and at the second, and a search for non-overlapping
// matches would miss the second. So, as for the generic matches, the search
// starts again one rune after the start of each match, not after its end.
// Each search keeps the rune before its start as context, so ^, \b and the
// other assertions keep the meaning they have in the whole text.
func (ap allowPattern) eachMatch(text []byte, want func(start int) bool, visit func(loc []int) bool) {
	for q := 0; q <= len(text); {
		loc := ap.leftmostFrom(text, q)
		if loc == nil || !want(loc[0]) || !visit(loc) {
			return
		}
		q = nextRune(text, loc[0])
	}
}

// leftmostFrom returns the leftmost match that starts at or after q.
func (ap allowPattern) leftmostFrom(text []byte, q int) []int {
	if q == 0 {
		return ap.rx.FindSubmatchIndex(text)
	}
	if q > len(text) {
		return nil
	}
	_, size := utf8.DecodeLastRune(text[:q])
	return shiftMatch(ap.after.FindSubmatchIndex(text[q-size:]), q-size, size)
}

// shiftMatch maps the submatch indexes of a `(?s:.)(?:P)` match in
// text[base:] to the indexes of the P match in text: the whole match starts
// one rune (size bytes) later, and each group moves by base.
func shiftMatch(loc []int, base, size int) []int {
	if loc == nil {
		return nil
	}
	out := make([]int, len(loc))
	for i, v := range loc {
		if v >= 0 {
			v += base
		}
		out[i] = v
	}
	out[0] += size
	return out
}

// nextRune returns the position of the rune after the one at pos.
func nextRune(text []byte, pos int) int {
	if pos >= len(text) {
		return pos + 1
	}
	_, size := utf8.DecodeRune(text[pos:])
	return pos + size
}

// Redact replaces every secret match in msg with "[REDACTED]". It ignores the
// allow list: a diagnostic message loses nothing it needs when a redirect
// parameter is hidden, and a redaction must not depend on context that a
// truncated message may have lost. Every pattern is matched against the
// original message, and overlapping matches are joined, so one replacement
// never hides a match from the next pattern.
func (m *Matcher) Redact(msg string) string {
	if m == nil || msg == "" {
		return msg
	}
	var spans [][2]int
	for _, rx := range m.secret {
		for _, loc := range rx.FindAllStringIndex(msg, -1) {
			if loc[1] > loc[0] {
				spans = append(spans, [2]int{loc[0], loc[1]})
			}
		}
	}
	if len(spans) == 0 {
		return msg
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	prev := 0
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] < end; i++ {
			if spans[i][1] > end {
				end = spans[i][1]
			}
		}
		b.WriteString(msg[prev:start])
		b.WriteString("[REDACTED]")
		prev = end
	}
	b.WriteString(msg[prev:])
	return b.String()
}
