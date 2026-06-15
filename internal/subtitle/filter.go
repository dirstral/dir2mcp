package subtitle

import "strings"

// WordFilter strips configured boilerplate/credits/watermark phrases from
// transcript and subtitle text before it is chunked/embedded (ingest) and on
// subtitle export (VTT/SRT). It is intentionally general-purpose: the phrase
// list comes entirely from configuration (media.filter_words) and is empty by
// default, so a zero/empty filter is a no-op everywhere.
//
// Matching is case-insensitive substring removal: every (case-insensitively)
// matched occurrence of a configured phrase is deleted from the text. A line or
// cue whose text is empty after removal is dropped by the callers (chunkers and
// renderers already skip empty text), so a phrase that constitutes an entire
// caption removes that caption.
//
// The same WordFilter is shared by the ingest chunkers and the export renderers
// so a phrase configured once is stripped consistently in both places.
type WordFilter struct {
	// lowered holds the lower-cased, non-empty phrases used for
	// case-insensitive matching, in configuration order.
	lowered []string
}

// NewWordFilter builds a WordFilter from a phrase list. Empty/whitespace-only
// phrases are skipped (they would otherwise match everywhere). The returned
// filter is deterministic and safe for concurrent read use. A nil/empty list
// yields a filter whose Active() is false and whose Apply is the identity.
func NewWordFilter(phrases []string) *WordFilter {
	f := &WordFilter{}
	for _, p := range phrases {
		if strings.TrimSpace(p) == "" {
			continue
		}
		f.lowered = append(f.lowered, strings.ToLower(p))
	}
	return f
}

// Active reports whether the filter has any phrases to strip. When false,
// callers can skip Apply entirely (it would be a no-op) so behavior is
// byte-identical to having no filter configured.
func (f *WordFilter) Active() bool {
	return f != nil && len(f.lowered) > 0
}

// Apply removes every case-insensitive occurrence of each configured phrase
// from text and returns the result with surrounding whitespace trimmed. When
// the filter is inactive it returns text unchanged. The result may be empty,
// which callers treat as "drop this line/cue".
//
// Phrases are applied in configuration order; removing one phrase can expose a
// later phrase, which is acceptable and deterministic.
func (f *WordFilter) Apply(text string) string {
	if !f.Active() {
		return text
	}
	for _, low := range f.lowered {
		text = removeAllFold(text, low)
	}
	return strings.TrimSpace(text)
}

// FilterCues applies the word filter to each cue's text and drops cues whose
// text is empty after filtering, re-indexing the survivors so the returned
// slice is gap-free (RenderSRT renumbers too, but re-indexing keeps Cue.Index
// meaningful for any other consumer). A nil/inactive filter returns cues
// unchanged (same elements, same order) so the empty-config export path is a
// no-op. Timing is preserved verbatim on surviving cues.
func FilterCues(cues []Cue, filter *WordFilter) []Cue {
	if !filter.Active() {
		return cues
	}
	out := make([]Cue, 0, len(cues))
	for _, cue := range cues {
		text := filter.Apply(cue.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		cue.Index = len(out) + 1
		cue.Text = text
		out = append(out, cue)
	}
	return out
}

// removeAllFold deletes every case-insensitive occurrence of phraseLow (already
// lower-cased) from s, preserving the casing of the surviving text.
//
// It folds s rune-by-rune into a lower-cased copy while recording, for every
// byte position in the folded copy, the originating byte offset in s. Folding a
// rune can change its byte length (e.g. some non-ASCII characters), so this
// per-byte map keeps match offsets exact rather than assuming length-preserving
// folding.
func removeAllFold(s, phraseLow string) string {
	if phraseLow == "" {
		return s
	}
	low, srcOf := foldWithOffsets(s)
	var out strings.Builder
	searchFrom := 0 // byte offset into low
	srcCursor := 0  // byte offset into s, last copied position
	for {
		rel := strings.Index(low[searchFrom:], phraseLow)
		if rel < 0 {
			out.WriteString(s[srcCursor:])
			break
		}
		matchLowStart := searchFrom + rel
		matchLowEnd := matchLowStart + len(phraseLow)
		srcStart := srcOf[matchLowStart]
		srcEnd := srcOf[matchLowEnd]
		if srcStart < srcCursor {
			srcStart = srcCursor
		}
		out.WriteString(s[srcCursor:srcStart])
		srcCursor = srcEnd
		searchFrom = matchLowEnd
	}
	return out.String()
}

// foldWithOffsets returns the lower-cased form of s together with a slice srcOf
// of length len(folded)+1 where srcOf[i] is the byte offset in s that the i-th
// byte of the folded string originates from (and srcOf[len] == len(s)). This
// lets callers map a match span in the folded string back to an exact byte span
// in the original.
func foldWithOffsets(s string) (folded string, srcOf []int) {
	var b strings.Builder
	srcOf = make([]int, 0, len(s)+1)
	for i, r := range s {
		lr := strings.ToLower(string(r))
		// Each byte of the folded rune maps back to the rune's start in s. Match
		// boundaries always fall on rune starts in s, so mapping every folded
		// byte of a rune to its source start is sufficient and exact.
		for j := 0; j < len(lr); j++ {
			srcOf = append(srcOf, i)
		}
		b.WriteString(lr)
	}
	srcOf = append(srcOf, len(s))
	return b.String(), srcOf
}
