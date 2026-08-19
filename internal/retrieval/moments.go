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

// primary returns the index of the best-ranked member. That member fixes the
// moment's rank and supplies the header of its context block; the block's text
// comes from every member (momentContextText). It returns -1 for a moment with
// no member, which
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

// momentContextText renders the prompt text of one moment and reports the hit
// indices of the members that text came from.
//
// EVERY member is placed, not the best-ranked one alone (issue #890). The first
// version of this file placed the primary and cited all the members, on the
// assumption that a moment's members report one event once per role, so the
// siblings only repeat what the primary already says. On a recognition corpus
// built from a structured feed the members are complementary, not redundant:
// one home run carries the batter in `at_bat`, the exit velocity and the
// distance in `batted_ball`, the index in `captivating` and the running score in
// `scoring_play`. Dropping the siblings dropped the facts, and citing them
// anyway made the answer name text the model never read, which SPEC §9.4.2
// forbids and which the truncation path of issue #403 already refuses to do.
//
// The moment stays ONE context block, so the #784 fix holds: the generator reads
// one document per event and cannot count a role-split annotation twice.
//
// Two rules keep the block honest and bounded:
//
//   - A member whose text repeats an already-placed member's text VERBATIM adds
//     nothing, so it is written once. It stays cited, because that one copy is
//     its text. Only exact repetition collapses; a near-duplicate is kept,
//     because on this corpus the sibling that looks like a paraphrase is the one
//     carrying the numbers.
//   - budget bounds the block's text in runes. Members are added in rank order
//     while they fit, and a member left out by the budget is NOT reported as
//     placed, so it is not cited either. The first text-carrying member is
//     always placed: an over-budget one is windowed by the caller exactly as a
//     single long chunk is, which keeps the matched region in the prompt
//     (§9.4.2).
//
// A member with no text at all (a media-only hit) places nothing and stays
// cited: the block quotes the same seconds of the same file, and no claim is
// made about text the model did not read.
func momentContextText(hits []model.SearchHit, fullTexts []string, m moment, budget int) (string, []int) {
	var b strings.Builder
	placed := make([]int, 0, len(m.members))
	seen := make(map[string]struct{}, len(m.members))
	used := 0
	for _, idx := range m.members {
		if idx < 0 || idx >= len(hits) {
			continue
		}
		text := memberText(hits, fullTexts, idx)
		if text == "" {
			placed = append(placed, idx)
			continue
		}
		if _, dup := seen[text]; dup {
			placed = append(placed, idx)
			continue
		}
		cost := len([]rune(text))
		if b.Len() > 0 {
			cost++ // the newline that separates two members
		}
		if b.Len() > 0 && used+cost > budget {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		used += cost
		seen[text] = struct{}{}
		placed = append(placed, idx)
	}
	return b.String(), placed
}

// memberText returns the prompt text of one member: its resolved full chunk
// text, or the hit's store snippet when the full text is unavailable, or "" for
// a member that carries no text at all.
func memberText(hits []model.SearchHit, fullTexts []string, idx int) string {
	if idx < 0 || idx >= len(hits) {
		return ""
	}
	if text := strings.TrimSpace(docTextAt(fullTexts, idx)); text != "" {
		return text
	}
	return strings.TrimSpace(hits[idx].Snippet)
}

// membersInSnippet narrows the members momentContextText selected to the ones
// whose text SURVIVED rendering, and it is what makes the citation set a subset
// of the prompt rather than of the builder's intent (issue #890).
//
// Rendering can still remove a member's text after the join: evidence
// compression (issue #335) drops sentences, marker neutralization (issue #445)
// grows adversarial text past the budget it was measured against, and the
// match-centered window then cuts the tail. A member the prompt no longer
// quotes is not citable.
//
// Two members stay regardless. A member with no text quotes nothing to look
// for, and it cites the same seconds of the same file as the block. The primary
// defines the block and is cited exactly as a lone chunk is: its window is
// match-centered, so it carries the region the citation names (SPEC §9.4.2),
// and narrowing it here would drop the citation of every compressed single-hit
// document.
func membersInSnippet(hits []model.SearchHit, fullTexts []string, primary int, placed []int, snippet string) []int {
	out := make([]int, 0, len(placed))
	for _, idx := range placed {
		text := memberText(hits, fullTexts, idx)
		if idx == primary || text == "" || strings.Contains(snippet, neutralizeRAGMarkers(text)) {
			out = append(out, idx)
		}
	}
	return out
}

// sortedIndices returns the given hit indices in ascending order. The answer's
// citations follow the retrieval order, not the order the prompt placed them.
func sortedIndices(idx []int) []int {
	sort.Ints(idx)
	return idx
}
