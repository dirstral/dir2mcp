package ingest

import (
	"context"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Re-timing of stored chat-engine translations (SPEC §8.6.2, dir2mcp #1133).
//
// A chat-engine translation produced before the "source-spoken" rule carries
// the older spans: each translated line ended where the next line started, so
// the silence after a source cue sat inside the translated segment and
// broadcast re-segmentation spread the translated words across it. The spec
// requires such a representation to be re-timed in a later reconcile pass from
// its stored text and the source transcript's word timings, without a model
// call. This file is that pass. It changes span rows only: the chunk rows, the
// text, the embeddings and the derivation identity are untouched, and the
// representation records `span_rule: "source-spoken"` once re-timed so the pass
// never looks at it again.

// translationRetimer is the optional store capability the pass needs. A store
// without it leaves stored translations as they are.
type translationRetimer interface {
	ChunkTimeSpans(ctx context.Context, repID int64) ([]store.ChunkTimeSpan, error)
	RetimeRepresentation(ctx context.Context, repID int64, metaJSON string, updates []store.ChunkSpanUpdate) error
}

// retimeSplitOverlapRunes is the shortest shared text between the end of one
// chunk and the start of the next that identifies them as two parts of one
// split line (chunkTextByChars overlaps parts by TranscriptChunkOverlapChars).
// Two distinct lines never share that much text at their boundary.
const retimeSplitOverlapRunes = 40

// retimeStoredTranslations re-times every stored chat-engine translation of the
// document at relPath that does not yet carry the span_rule marker. skip names
// representations this pass must not touch (the ones just retired).
func (s *Service) retimeStoredTranslations(ctx context.Context, relPath string, reps []store.RepresentationRow, skip map[int64]struct{}) {
	retimer, ok := s.store.(translationRetimer)
	if !ok {
		return
	}
	for _, rep := range reps {
		if _, retired := skip[rep.RepID]; retired {
			continue
		}
		meta, ok := translationMetaToRetime(rep)
		if !ok {
			continue
		}
		s.retimeOneTranslation(ctx, retimer, relPath, rep, reps, meta)
	}
}

// translationMetaToRetime reports whether rep is a stored chat-engine
// translation without the span_rule marker, and returns its meta as a generic
// object so the rewrite preserves every recorded field. A whisper-engine
// translation carries per-word timing of its own (meta words=true) and is not
// re-timed (SPEC §8.6.2).
func translationMetaToRetime(rep store.RepresentationRow) (map[string]any, bool) {
	if _, isTranslation := translatedTranscriptLanguage(rep); !isTranslation {
		return nil, false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(rep.MetaJSON), &meta); err != nil || meta == nil {
		return nil, false
	}
	if rule, _ := meta["span_rule"].(string); rule != "" {
		return nil, false
	}
	if words, _ := meta["words"].(bool); words {
		return nil, false
	}
	return meta, true
}

// retimeOneTranslation re-times rep from the words of its source transcript
// (the transcript of the same track) and records the marker. Without source
// words the spans stay as they are and only the marker is recorded, so the
// pass does not revisit a representation it can never improve.
func (s *Service) retimeOneTranslation(ctx context.Context, retimer translationRetimer, relPath string, rep store.RepresentationRow, reps []store.RepresentationRow, meta map[string]any) {
	track, _ := meta["track"].(float64)
	sourceRepType := TranscriptRepTypeForTrack(int(track), "")
	var sourceRepID int64
	for _, cand := range reps {
		if cand.RepType == sourceRepType {
			sourceRepID = cand.RepID
			break
		}
	}
	if sourceRepID == 0 {
		s.getLogger().Printf("translation re-time: %s %s has no live %s source transcript; left as stored (#1133)", relPath, rep.RepType, sourceRepType)
		return
	}
	// Every failure below marks the scan incomplete, so the identity stays
	// unrecorded and the next scan retries this representation.
	sourceChunks, err := retimer.ChunkTimeSpans(ctx, sourceRepID)
	if err != nil {
		s.getLogger().Printf("translation re-time: read source spans for %s %s failed: %v", relPath, rep.RepType, err)
		s.reconcileIncomplete = true
		return
	}
	words := timedWordsOfChunks(sourceChunks)
	var updates []store.ChunkSpanUpdate
	if len(words) > 0 {
		chunks, err := retimer.ChunkTimeSpans(ctx, rep.RepID)
		if err != nil {
			s.getLogger().Printf("translation re-time: read spans for %s %s failed: %v", relPath, rep.RepType, err)
			s.reconcileIncomplete = true
			return
		}
		updates = retimeTranslatedChunks(chunks, words)
	}
	meta["span_rule"] = translationSpanRuleSourceSpoken
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		s.getLogger().Printf("translation re-time: marshal meta for %s %s failed: %v", relPath, rep.RepType, err)
		s.reconcileIncomplete = true
		return
	}
	if err := retimer.RetimeRepresentation(ctx, rep.RepID, string(metaJSON), updates); err != nil {
		s.getLogger().Printf("translation re-time: write %s %s failed: %v", relPath, rep.RepType, err)
		s.reconcileIncomplete = true
		return
	}
	s.getLogger().Printf("translation re-time: %s %s now carries the spoken spans of its source lines (%d chunk spans moved from %d source words) (#1133)",
		relPath, rep.RepType, len(updates), len(words))
}

// timedWordsOfChunks flattens the per-word timing recorded on a transcript's
// spans into TimedWords sorted by start, the form spokenEndInWindow reads.
func timedWordsOfChunks(chunks []store.ChunkTimeSpan) []model.TimedWord {
	var words []model.TimedWord
	for _, ch := range chunks {
		for _, w := range ch.Span.Words {
			words = append(words, model.TimedWord{Word: w.W, StartMS: w.T, EndMS: w.T + w.D})
		}
	}
	sort.SliceStable(words, func(i, j int) bool { return words[i].StartMS < words[j].StartMS })
	return words
}

// spokenEndInWindow returns the latest end among the words that start inside
// [startMS, endMS), and whether any word starts there. With cap the end never
// passes endMS: the window's end is the next source marker, which the spoken
// end never crosses (SPEC §8.6.2). The last window of a transcript has no next
// marker, only the chunker's pace estimate, and the source words are the
// better measure of where it ends, so its caller passes cap=false, as
// spokenLineEnds does for a fresh translation. words must be sorted by start.
func spokenEndInWindow(words []model.TimedWord, startMS, endMS int, cap bool) (int, bool) {
	first := sort.Search(len(words), func(i int) bool { return words[i].StartMS >= startMS })
	end, found := 0, false
	for i := first; i < len(words) && words[i].StartMS < endMS; i++ {
		found = true
		if words[i].EndMS > end {
			end = words[i].EndMS
		}
	}
	if !found {
		return 0, false
	}
	if cap && end > endMS {
		end = endMS
	}
	return end, true
}

// retimeTranslatedChunks computes the span updates that give each translated
// chunk the spoken span of its window (SPEC §8.6.2). A chunk's window is the
// stored span, whose end under the older rule is the next source line's start,
// so the spoken end never crosses the next marker. A merged retrieval window
// (SPEC 8.6.1) carries one cue per source line and is re-timed cue by cue; its
// span end follows its last cue. The parts of one line that chunkTextByChars
// split share one window and redistribute the spoken span by rune weight, as
// splitTranscriptSegmentWithTiming does. A window with no source words, or
// whose words end at or past its end, is left as it is.
func retimeTranslatedChunks(chunks []store.ChunkTimeSpan, words []model.TimedWord) []store.ChunkSpanUpdate {
	var updates []store.ChunkSpanUpdate
	for i := 0; i < len(chunks); {
		ch := chunks[i]
		if ch.Span.Kind != "time" {
			i++
			continue
		}
		if len(ch.Span.Cues) > 1 {
			if span, changed := retimeMergedWindow(ch.Span, words, i == len(chunks)-1); changed {
				updates = append(updates, store.ChunkSpanUpdate{ChunkID: ch.ChunkID, Span: span})
			}
			i++
			continue
		}
		j := i
		for j+1 < len(chunks) && isSplitContinuation(chunks[j], chunks[j+1]) {
			j++
		}
		updates = append(updates, retimeSplitGroup(chunks[i:j+1], words, j == len(chunks)-1)...)
		i = j + 1
	}
	return updates
}

// retimeMergedWindow re-times each recorded cue of a merged chunk window to the
// spoken end of the source words that start inside it, and moves the window's
// end to its last cue's end. last marks the transcript's final window, whose
// final cue has no next marker to cap it.
func retimeMergedWindow(span model.Span, words []model.TimedWord, last bool) (model.Span, bool) {
	cues := append([]model.CueSpan(nil), span.Cues...)
	changed := false
	end := span.StartMS
	for k := range cues {
		cueEnd := cues[k].T + cues[k].D
		capped := !last || k != len(cues)-1
		if spoken, ok := spokenEndInWindow(words, cues[k].T, cueEnd, capped); ok && spoken > cues[k].T && spoken != cueEnd {
			cues[k].D = spoken - cues[k].T
			cueEnd = spoken
			changed = true
		}
		if cueEnd > end {
			end = cueEnd
		}
	}
	if end <= span.StartMS {
		end = span.StartMS + 1
	}
	if !changed && end == span.EndMS {
		return span, false
	}
	out := span
	out.Cues = cues
	out.EndMS = end
	return out, true
}

// retimeSplitGroup re-times one line's window, held by one chunk or by the
// parts chunkTextByChars split it into. last marks the transcript's final
// window, which has no next marker to cap it.
func retimeSplitGroup(group []store.ChunkTimeSpan, words []model.TimedWord, last bool) []store.ChunkSpanUpdate {
	startMS := group[0].Span.StartMS
	endMS := group[len(group)-1].Span.EndMS
	spoken, ok := spokenEndInWindow(words, startMS, endMS, !last)
	if !ok || spoken <= startMS || spoken == endMS {
		return nil
	}
	if len(group) == 1 {
		span := group[0].Span
		span.EndMS = spoken
		return []store.ChunkSpanUpdate{{ChunkID: group[0].ChunkID, Span: span}}
	}
	return redistributeSplitGroup(group, startMS, spoken)
}

// redistributeSplitGroup gives the parts of one split line the window
// [startMS, endMS) by rune weight, the rule of splitTranscriptSegmentWithTiming.
func redistributeSplitGroup(group []store.ChunkTimeSpan, startMS, endMS int) []store.ChunkSpanUpdate {
	counts := make([]int, len(group))
	total := 0
	for i, ch := range group {
		counts[i] = utf8.RuneCountInString(ch.Text)
		total += counts[i]
	}
	duration := endMS - startMS
	updates := make([]store.ChunkSpanUpdate, 0, len(group))
	cum := 0
	for i, ch := range group {
		var partStart, partEnd int
		if total == 0 {
			partStart = startMS + (duration*i)/len(group)
			partEnd = startMS + (duration*(i+1))/len(group)
		} else {
			partStart = startMS + (duration*cum)/total
			cum += counts[i]
			partEnd = startMS + (duration*cum)/total
		}
		if partEnd <= partStart {
			partEnd = partStart + 1
		}
		span := ch.Span
		span.StartMS = partStart
		span.EndMS = partEnd
		updates = append(updates, store.ChunkSpanUpdate{ChunkID: ch.ChunkID, Span: span})
	}
	return updates
}

// isSplitContinuation reports whether next is a further part of the line prev
// holds a part of: both are plain time spans (no merged cues), next starts
// where prev ends, and the two texts share the chunker's overlap at the
// boundary (a suffix of prev that is a prefix of next, at least
// retimeSplitOverlapRunes long).
func isSplitContinuation(prev, next store.ChunkTimeSpan) bool {
	if prev.Span.Kind != "time" || next.Span.Kind != "time" {
		return false
	}
	if len(prev.Span.Cues) > 1 || len(next.Span.Cues) > 1 {
		return false
	}
	if next.Span.StartMS != prev.Span.EndMS {
		return false
	}
	a, b := []rune(prev.Text), []rune(next.Text)
	// The chunker overlaps parts by TranscriptChunkOverlapChars and may snap the
	// cut to a word boundary, so twice that length bounds the shared text.
	limit := 2 * TranscriptChunkOverlapChars
	if len(a) < limit {
		limit = len(a)
	}
	if len(b) < limit {
		limit = len(b)
	}
	for k := limit; k >= retimeSplitOverlapRunes; k-- {
		if string(a[len(a)-k:]) == string(b[:k]) {
			return true
		}
	}
	return false
}

// RetimeTranslatedChunksForTest exposes retimeTranslatedChunks for the tests
// tree.
func RetimeTranslatedChunksForTest(chunks []store.ChunkTimeSpan, words []model.TimedWord) []store.ChunkSpanUpdate {
	return retimeTranslatedChunks(chunks, words)
}
