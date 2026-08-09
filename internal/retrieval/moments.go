package retrieval

import (
	"sort"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Moment grouping for the answer path (issue #784).
//
// A recognition backend reports one moment once per role. The reference backend
// reports one pitch twice: once as `pitch` keyed on the pitcher, and once as
// `at_bat` keyed on the batter. The two annotations carry the same time span on
// the same file.
//
// That split is deliberate, and it must stay. The v1 wire shape holds a flat
// entity array and one scalar event, so one annotation cannot record that this
// id pitched and that id batted. Role lives in the granularity of the
// annotations alone. A merged annotation destroys the role-exact entity filter
// (dirstral-spec design 0004 section 8, and the filter of PR #789).
//
// The cost of the split falls on the answer. Each annotation becomes its own
// context document, so the generator reads one event as two events and counts
// it twice. On the pilot corpus, 312 of 346 chunks were such pairs, and `ask`
// reported that a player "walked twice" when he walked once.
//
// Design 0004 section 8 names the remedy that stays inside v1: keep the
// annotations separate, and group them on the shared time span at answer time.
// This file does that grouping. It changes the prompt only. Search results,
// their order, the hits on the ask result, and every span stay as they are.

// moment holds each candidate that reports one event. members are indices into
// the hit slice, in rank order.
type moment struct {
	members []int
}

// primary returns the index of the best-ranked member. That member represents
// the moment in the prompt. It returns -1 for a moment with no member, which
// groupMoments never builds, so a caller can range over any moment slice
// safely.
func (m moment) primary() int {
	if len(m.members) == 0 {
		return -1
	}
	return m.members[0]
}

// momentKey returns the group key of a recognition annotation. It also reports
// whether the hit is one.
//
// Only a recognition annotation groups. A hit with no attribution keeps its own
// moment, even if its span equals the span of another hit. Two transcript
// segments, or an OCR window and a transcript window over the same seconds,
// carry different text. To collapse them would discard evidence.
//
// A span with no end is unknown, so it does not group either. Grouping on an
// unknown span would put unrelated annotations into one moment.
func momentKey(h model.SearchHit) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(h.Span.Kind), "time") {
		return "", false
	}
	if strings.TrimSpace(h.Span.Event) == "" && len(h.Span.Entities) == 0 {
		return "", false
	}
	if h.Span.EndMS <= 0 || h.Span.EndMS < h.Span.StartMS {
		return "", false
	}
	return h.RelPath + "\x00" + strconv.Itoa(h.Span.StartMS) + "\x00" + strconv.Itoa(h.Span.EndMS), true
}

// groupMoments collects the hits that report one event into one moment each.
// Two hits share a moment when both carry a recognition attribution and their
// (rel_path, start_ms, end_ms) are equal. Every other hit becomes a moment of
// one member, so a corpus without recognition annotations groups into exactly
// the hit list it was given.
//
// The result keeps the rank order of the hits: a moment holds the position of
// its best-ranked member, and its members stay in rank order.
func groupMoments(hits []model.SearchHit) []moment {
	out := make([]moment, 0, len(hits))
	byKey := make(map[string]int, len(hits))
	for i, h := range hits {
		key, ok := momentKey(h)
		if !ok {
			out = append(out, moment{members: []int{i}})
			continue
		}
		if at, seen := byKey[key]; seen {
			out[at].members = append(out[at].members, i)
			continue
		}
		byKey[key] = len(out)
		out = append(out, moment{members: []int{i}})
	}
	return out
}

// momentMemberIndices returns the hit indices of the given moments, in rank
// order. The answer cites every member of each moment it used, because each
// member is real provenance for the same seconds of the same file. Only the
// count of events changes, not the evidence a caller can open.
func momentMemberIndices(moments []moment) []int {
	out := make([]int, 0, len(moments))
	for _, m := range moments {
		out = append(out, m.members...)
	}
	sort.Ints(out)
	return out
}
