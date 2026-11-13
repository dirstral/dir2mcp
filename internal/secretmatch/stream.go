package secretmatch

import (
	"regexp"
	"sort"
)

const (
	// StreamOverlapBytes is the number of bytes a Stream keeps from one window
	// into the next when no match needs more. A secret match that lands on a
	// window boundary is then inside the next window whole: 4 KiB is far longer
	// than the shortest match of any credential pattern the product ships or an
	// operator writes.
	StreamOverlapBytes = 4 << 10

	// StreamMaxRetainBytes bounds the bytes a Stream keeps between two writes.
	// A Stream keeps more than StreamOverlapBytes only to avoid a cut through a
	// match, or to defer a decision that depends on bytes it has not seen yet
	// (see Stream). When that needs more than this bound, the Stream reports a
	// match: it refuses rather than decide on a part of the text.
	StreamMaxRetainBytes = 256 << 10

	// streamMinJudgeBytes is the number of new bytes a Stream collects before
	// it judges a window. A window re-runs the patterns over the kept bytes, so
	// judging on every tiny write would cost the kept size per write.
	streamMinJudgeBytes = 8 << 10
)

// Stream applies a Matcher to text that arrives in pieces, with bounded
// memory, and reaches the decision Match would reach on the whole text.
//
// Each Write judges a window: the bytes the Stream kept plus the new bytes. Two
// rules keep the window decision equal to the whole-text decision:
//
//   - A generic token match is decided as cancelled only when an allow match
//     covers it and that allow match ends before the window end. Any other
//     generic token match is deferred, because bytes after the window can
//     change the answer: an allow match that ends at the window end can shrink
//     or vanish (an anchored `$`, or a value that runs on into a character the
//     allow pattern does not accept), a generic token match that ends there can
//     grow, and an allow match that needs more bytes can still complete. The
//     Stream keeps the text from the start of the covering allow match, or
//     StreamOverlapBytes before the generic token match, and judges it again
//     with the next bytes. Close judges the last window with its end as the end
//     of the text, and then an uncovered match is final.
//   - A window never starts inside a match. The Stream cuts its kept text at a
//     position that no generic token match and no allow match crosses, so the
//     next window sees each such match whole and from its real start. An allow
//     match that starts at the first byte of a later window is trusted only when
//     the Stream cut there because it found that allow match with its real left
//     context; otherwise a `^` in an allow pattern could match at a cut that is
//     not the start of the text.
//
// A match of a secret pattern that no allow match can cancel is final at once.
// One drift stays, and it is fail-closed: a secret pattern anchored with `$`
// (or `\z`) can match at the end of a window that is not the end of the text,
// and one anchored with `^` (or `\A`) can match at a cut. Both can only refuse
// text that Match would serve; neither can serve text that Match refuses. A
// secret pattern match at the first byte of a later window that lies wholly in
// bytes an earlier window judged is not counted again, so a `^` at a cut does
// not refuse text the earlier window cleared.
//
// A Stream is not safe for concurrent use.
type Stream struct {
	m *Matcher
	// buf holds the kept bytes followed by the newest write.
	buf []byte
	// base is the offset of buf[0] in the text.
	base int64
	// judged is the number of bytes at the head of buf that an earlier window
	// judged.
	judged int
	// allowAtZero reports that buf[0] starts an allow match that an earlier
	// window found with its real left context.
	allowAtZero bool
	pending     bool
	matched     bool
	maxRetain   int
}

// NewStream returns a Stream over m. A nil or disabled matcher yields a Stream
// that never matches.
func (m *Matcher) NewStream() *Stream {
	return &Stream{m: m, maxRetain: StreamMaxRetainBytes}
}

// SetMaxRetain overrides StreamMaxRetainBytes. It is for tests.
func (s *Stream) SetMaxRetain(n int) {
	if n > 0 {
		s.maxRetain = n
	}
}

// Matched reports whether the Stream has found a secret match that no allow
// match cancels. The result is sticky.
func (s *Stream) Matched() bool { return s.matched }

// Pending reports that the last window ended inside a cancellation the Stream
// could not decide yet. A caller that stops before the end of the text while a
// decision is pending must treat the text as matched, or feed more bytes.
func (s *Stream) Pending() bool { return s.pending || len(s.buf) > s.judged }

// Write judges p together with the kept bytes. It returns true when the text
// so far holds a secret match that no allow match cancels. A write smaller than
// streamMinJudgeBytes is collected and judged with the next one, or by Close;
// Pending reports true while such bytes wait.
func (s *Stream) Write(p []byte) bool {
	if s.matched || !s.m.Enabled() || len(p) == 0 {
		return s.matched
	}
	s.buf = append(s.buf, p...)
	if len(s.buf)-s.judged < streamMinJudgeBytes {
		return false
	}
	s.judge(false)
	return s.matched
}

// Close judges the kept bytes with their end as the end of the text, so every
// deferred decision is made. It returns the final result.
func (s *Stream) Close() bool {
	if s.matched || !s.m.Enabled() || len(s.buf) == 0 {
		s.pending = false
		return s.matched
	}
	s.judge(true)
	s.pending = false
	s.judged = len(s.buf)
	return s.matched
}

// windowState is what one window judgement found.
type windowState struct {
	text      []byte
	atStart   bool
	final     bool
	allow     [][]int
	allowDone bool
	// intervals holds the generic token matches; the cut must not cross them.
	intervals [][]int
	// pendStart is the earliest position a deferred decision needs, or -1.
	pendStart int
}

func (s *Stream) judge(final bool) {
	w := &windowState{text: s.buf, atStart: s.base == 0, final: final, pendStart: -1}
	for _, rx := range s.m.secret {
		if !s.m.cancellable(rx) {
			if s.uncancellableMatch(rx, w.text, w.atStart) {
				s.matched = true
				return
			}
			continue
		}
		if s.judgeGeneric(rx, w) {
			s.matched = true
			return
		}
	}
	s.pending = w.pendStart >= 0
	if final {
		return
	}
	s.keepTail(w)
}

// judgeGeneric judges the matches of the generic token pattern rx in the
// window. It returns true on a match that no allow match can cancel, and
// otherwise records the matches and any deferred decision in w.
func (s *Stream) judgeGeneric(rx *regexp.Regexp, w *windowState) bool {
	matches := rx.FindAllIndex(w.text, -1)
	if len(matches) == 0 {
		return false
	}
	if !w.allowDone {
		w.allow = s.trustedAllowSpans(w.text, w.atStart)
		w.allowDone = true
	}
	uncertainEnd := len(w.text)
	if w.final {
		uncertainEnd = -1
	}
	cover := newCoverIndex(w.allow, uncertainEnd)
	for _, loc := range matches {
		if cover.covers(loc[0], loc[1]) {
			continue
		}
		if w.final {
			return true
		}
		// Not covered yet. Bytes after the window can still complete an allow
		// match that covers it, or end an allow match that covers it now at the
		// window end. Defer, and keep enough text before the match for an allow
		// match that starts before it.
		start := loc[0] - StreamOverlapBytes
		if start < 0 {
			start = 0
		}
		if u := cover.uncertainCover(loc[0]); u >= 0 && u < start {
			start = u
		}
		if w.pendStart < 0 || start < w.pendStart {
			w.pendStart = start
		}
	}
	w.intervals = append(w.intervals, matches...)
	return false
}

// keepTail keeps the text the next window needs: the last
// StreamOverlapBytes, moved back so that no match is cut and every deferred
// decision keeps its text. When that is more than maxRetain, it reports a
// match.
func (s *Stream) keepTail(w *windowState) {
	n := len(w.text)
	if !w.allowDone && len(s.m.allow) > 0 {
		// The kept text must not cut an allow match either, so the next window
		// sees it from its real start.
		w.allow = s.trustedAllowSpans(w.text, w.atStart)
	}
	cut := n - StreamOverlapBytes
	if cut < 0 {
		cut = 0
	}
	if w.pendStart >= 0 && w.pendStart < cut {
		cut = w.pendStart
	}
	cut = safeCut(append(w.intervals, w.allow...), cut)
	if n-cut > s.maxRetain {
		s.matched = true
		return
	}
	s.allowAtZero = false
	if cut > 0 || !w.atStart {
		for _, span := range w.allow {
			if span[0] == cut {
				s.allowAtZero = true
				break
			}
		}
	}
	kept := copy(s.buf, s.buf[cut:])
	s.buf = s.buf[:kept]
	s.base += int64(cut)
	s.judged = kept
}

// uncancellableMatch reports whether rx, a secret pattern no allow match can
// cancel, matches the window. A match at the first byte of a later window that
// lies wholly in bytes an earlier window judged is skipped: that window saw
// those bytes with their real left context, and a `^` in rx can match at a cut.
func (s *Stream) uncancellableMatch(rx *regexp.Regexp, text []byte, atStart bool) bool {
	if !rx.Match(text) {
		return false
	}
	if atStart || s.judged == 0 {
		return true
	}
	for _, loc := range rx.FindAllIndex(text, -1) {
		if loc[0] == 0 && loc[1] <= s.judged {
			continue
		}
		return true
	}
	return false
}

// trustedAllowSpans returns the allow matches in the window. A match at the
// first byte of a later window is kept only when an earlier window found an
// allow match that starts there.
func (s *Stream) trustedAllowSpans(text []byte, atStart bool) [][]int {
	spans := s.m.allowSpans(text)
	if atStart || s.allowAtZero {
		return spans
	}
	out := spans[:0]
	for _, span := range spans {
		if span[0] == 0 {
			continue
		}
		out = append(out, span)
	}
	return out
}

// safeCut returns the largest position at or before cut that no interval
// crosses (start < position < end).
func safeCut(intervals [][]int, cut int) int {
	if len(intervals) == 0 || cut <= 0 {
		return cut
	}
	sorted := append([][]int(nil), intervals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	// Merge the intervals into clusters, then move cut to the start of the
	// cluster that holds it.
	clusterStart, clusterEnd := sorted[0][0], sorted[0][1]
	for _, iv := range sorted[1:] {
		if iv[0] < clusterEnd {
			if iv[1] > clusterEnd {
				clusterEnd = iv[1]
			}
			continue
		}
		if clusterStart < cut && cut < clusterEnd {
			return clusterStart
		}
		clusterStart, clusterEnd = iv[0], iv[1]
	}
	if clusterStart < cut && cut < clusterEnd {
		return clusterStart
	}
	return cut
}
