package retrieval

import (
	"regexp"
	"strings"
	"unicode"
)

// Context sufficiency (SPEC §9.4.2, issue #403 F5).
//
// The text supplied to the model for a hit must contain the region its citation
// points at. A fixed HEAD truncation does not satisfy that: chunks run to
// structuredChunkMaxChars (2500), the store snippet is the first 240 runes and
// the prompt builder used to cut that again at 300, so a clause sitting past the
// truncation point was never seen by the model even though its citation invited
// the consumer to open that span and verify it.
//
// matchCenteredWindow selects the window of a chunk that actually carries the
// query match instead, and returns the whole chunk untouched whenever it fits
// the budget.

// ragWindowEllipsis marks a window that starts or ends mid-chunk, so the model
// can tell a partial window from a complete chunk.
const ragWindowEllipsis = "..."

// ragWindowEllipsisReserve is the rune budget held back for the two ellipsis
// markers a mid-chunk window may need on either side.
const ragWindowEllipsisReserve = 2 * len(ragWindowEllipsis)

// minQueryTermRunes is the shortest query token used to localize a window.
// One- and two-rune tokens are too common to carry positional information and
// would pull the window toward incidental matches.
const minQueryTermRunes = 3

// maxQueryTerms bounds the per-document scan: the window search is
// O(terms * runes), so a pathologically long question cannot make prompt
// assembly expensive.
const maxQueryTerms = 32

// queryTermRe splits a question into candidate match terms (letters and digits),
// mirroring mmrTokenRe's tokenization so the two relevance heuristics agree on
// what a word is.
var queryTermRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// matchCenteredWindow returns at most budget runes of text, centered on the
// densest cluster of query-term matches (SPEC §9.4.2). Text that already fits
// the budget is returned unchanged, so a chunk small enough to send whole is
// never windowed. When the question shares no term with the text there is no
// match to center on and the leading window is returned, which is the previous
// head-truncation behaviour and the best available guess.
//
// The result never exceeds budget runes, ellipsis markers included, so the
// caller can size a per-document budget exactly.
func matchCenteredWindow(text, query string, budget int) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= budget {
		return text
	}
	inner := budget
	if inner > ragWindowEllipsisReserve*2 {
		inner -= ragWindowEllipsisReserve
	}
	positions, weights := termMatchPositions(runes, queryMatchTerms(query))
	if len(positions) == 0 {
		return truncateSnippet(text, budget)
	}
	start := densestWindowStart(positions, weights, len(runes), inner)
	end := start + inner
	if end > len(runes) {
		end = len(runes)
	}
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = ragWindowEllipsis + out
	}
	if end < len(runes) {
		out += ragWindowEllipsis
	}
	return out
}

// queryMatchTerms reduces a question to the deduplicated lowercase tokens used
// to locate the window. Order is preserved so window selection is deterministic
// for a given question.
func queryMatchTerms(query string) []string {
	out := make([]string, 0, maxQueryTerms)
	seen := make(map[string]struct{}, maxQueryTerms)
	for _, tok := range queryTermRe.FindAllString(query, -1) {
		tok = strings.ToLower(tok)
		if len([]rune(tok)) < minQueryTermRunes {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
		if len(out) == maxQueryTerms {
			break
		}
	}
	return out
}

// termMatch is one occurrence of a query term in a chunk: its rune offset and
// the weight it contributes to a window's density score.
type termMatch struct {
	pos    int
	weight int
}

// termMatchPositions reports, in ascending order, the rune offsets in text at
// which a query term occurs, together with the weight of each match.
//
// The weight is the term's rune length. That is a deliberately language-neutral
// stand-in for term rarity: long tokens are rarer and more discriminative, so a
// window dense in them beats a window dense in short function words, without
// the codebase carrying a per-language stopword list.
//
// Matching is done rune-wise on a per-rune lowercase copy rather than on
// strings.ToLower output, because case folding can change a string's byte
// length and would desynchronize byte offsets from the rune offsets the caller
// slices with.
func termMatchPositions(runes []rune, terms []string) ([]int, []int) {
	if len(terms) == 0 {
		return nil, nil
	}
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	matches := make([]termMatch, 0, len(terms))
	for _, term := range terms {
		needle := []rune(term)
		for from := 0; from+len(needle) <= len(lower); {
			i := indexRunes(lower[from:], needle)
			if i < 0 {
				break
			}
			matches = append(matches, termMatch{pos: from + i, weight: len(needle)})
			from += i + 1
		}
	}
	sortMatchesByPos(matches)
	positions := make([]int, len(matches))
	weights := make([]int, len(matches))
	for i, m := range matches {
		positions[i] = m.pos
		weights[i] = m.weight
	}
	return positions, weights
}

// densestWindowStart returns the start offset of the window of the given size
// that maximizes the summed weight of the matches it covers, and centers that
// window on the covered cluster so the match is surrounded by context rather
// than pinned to an edge. Ties resolve to the earliest window, so selection is
// deterministic.
func densestWindowStart(positions, weights []int, textLen, window int) int {
	best, bestWeight, sum, j := 0, -1, 0, 0
	for i := range positions {
		if j < i {
			j, sum = i, 0
		}
		for j < len(positions) && positions[j] < positions[i]+window {
			sum += weights[j]
			j++
		}
		if sum > bestWeight {
			bestWeight = sum
			best = centerWindow(positions[i], positions[j-1], textLen, window)
		}
		sum -= weights[i]
	}
	return best
}

// centerWindow places a window of the given size around the cluster spanning
// [clusterStart, clusterEnd], clamped to the bounds of the text.
func centerWindow(clusterStart, clusterEnd, textLen, window int) int {
	slack := window - (clusterEnd - clusterStart)
	if slack < 0 {
		slack = 0
	}
	start := clusterStart - slack/2
	if start+window > textLen {
		start = textLen - window
	}
	if start < 0 {
		start = 0
	}
	return start
}

// indexRunes reports the offset of the first occurrence of needle in haystack,
// or -1. Both are already case-folded by the caller.
func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, r := range needle {
			if haystack[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// sortMatchesByPos sorts matches ascending by rune offset with an insertion
// sort: the slice holds at most one entry per term occurrence and is already
// grouped by term, so this stays cheap and keeps the ordering stable.
func sortMatchesByPos(matches []termMatch) {
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j-1].pos > matches[j].pos; j-- {
			matches[j-1], matches[j] = matches[j], matches[j-1]
		}
	}
}
