package ingest

import (
	"strings"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
)

// transcriptWindow is the SPEC 8.6.1 transcript chunk window.
//
// A transcript segment is one breath group, about eight seconds of speech from a
// whisper-class provider and one authored cue from a subtitle sidecar. Either
// way it is too fine a unit for retrieval over speech: a sentence splits across
// two chunks, and ten retrieved chunks cover eighty seconds of a three-hour
// recording (dir2mcp #955). The window merges consecutive segments into one
// chunk until adding the next would make the chunk longer than ChunkMS, or until
// the silence before the next segment is longer than GapMS.
//
// ChunkMS of 0 disables merging and restores one chunk per segment.
type transcriptWindow struct {
	ChunkMS int
	GapMS   int
}

func (w transcriptWindow) active() bool { return w.ChunkMS > 0 }

// mergeTranscriptChunkWindows applies the window to already-chunked, already-
// cleaned transcript segments.
//
// It runs AFTER the word filter and the cue cleaning, never before, for one
// reason: it records each member's rune length on the merged span so subtitle
// export can cut the merged text back into the original cues (SPEC 8.6.3). A
// pass that rewrote the text afterwards would invalidate those lengths, and the
// export would cut in the wrong places. Everything downstream of here
// (diarization, the meta, persistence) is metadata only.
//
// Only "time" spans merge. A non-time span, an out-of-order span and a span that
// would run backwards each close the current window instead of joining it, so a
// merged window's [start,end] never inverts.
func mergeTranscriptChunkWindows(segs []chunkSegment, w transcriptWindow) []chunkSegment {
	if !w.active() || len(segs) < 2 {
		return segs
	}
	out := make([]chunkSegment, 0, len(segs))
	var cur *chunkSegment
	flush := func() {
		if cur == nil {
			return
		}
		// A window of one is not a window: leave the segment exactly as it was,
		// with no cue metadata, so an unmerged transcript round-trips unchanged.
		if len(cur.Span.Cues) == 1 {
			cur.Span.Cues = nil
		}
		out = append(out, *cur)
		cur = nil
	}
	for i := range segs {
		seg := segs[i]
		if !isMergeableTimeSpan(seg) {
			flush()
			out = append(out, seg)
			continue
		}
		if cur == nil {
			cur = startWindow(seg)
			continue
		}
		if !windowAccepts(*cur, seg, w) {
			flush()
			cur = startWindow(seg)
			continue
		}
		joinWindow(cur, seg)
	}
	flush()
	return out
}

func isMergeableTimeSpan(seg chunkSegment) bool {
	return strings.EqualFold(strings.TrimSpace(seg.Span.Kind), "time") &&
		seg.Span.EndMS >= seg.Span.StartMS &&
		strings.TrimSpace(seg.Text) != ""
}

// startWindow opens a window on one segment, recording that segment as the
// window's first exportable cue.
func startWindow(seg chunkSegment) *chunkSegment {
	cur := seg
	cur.Span.Cues = []model.CueSpan{{
		T: seg.Span.StartMS,
		D: seg.Span.EndMS - seg.Span.StartMS,
		N: utf8.RuneCountInString(seg.Text),
	}}
	return &cur
}

// windowAccepts reports whether seg joins the open window.
//
// The duration rule is measured on the span the merged window would have. The
// silence rule is measured from the window's current end, which is exact when
// the transcript carries real per-segment timing and is the chunker's estimated
// end otherwise; either way it separates a pause from a continuous turn.
//
// A speaker change always closes the window. A speaker turn is already the right
// retrieval unit, and merging across one would attribute a chunk to a speaker
// who did not say half of it. A language change closes it for the same reason
// (SPEC §8.2.2): a chunk that mixed two languages would be matched by the §9.5
// filter on a language half its text is not in.
func windowAccepts(cur, seg chunkSegment, w transcriptWindow) bool {
	if seg.Span.StartMS < cur.Span.StartMS {
		return false
	}
	if !sameSpeaker(cur.Span, seg.Span) {
		return false
	}
	if !sameSpanLanguage(cur.Span, seg.Span) {
		return false
	}
	if seg.Span.EndMS-cur.Span.StartMS > w.ChunkMS {
		return false
	}
	if w.GapMS > 0 && seg.Span.StartMS-cur.Span.EndMS > w.GapMS {
		return false
	}
	return true
}

func sameSpeaker(a, b model.Span) bool {
	return strings.TrimSpace(a.Speaker) == strings.TrimSpace(b.Speaker) &&
		strings.TrimSpace(a.SpeakerLabel) == strings.TrimSpace(b.SpeakerLabel)
}

// sameSpanLanguage is the fourth close rule of SPEC §8.2.2: a chunk window never
// spans a language change, so a chunk has one language and its segment
// language is also the chunk's. An empty Language means "the representation's
// language", so two unmarked segments agree and a marked one differs from them.
func sameSpanLanguage(a, b model.Span) bool {
	return strings.EqualFold(strings.TrimSpace(a.Language), strings.TrimSpace(b.Language))
}

// joinWindow appends seg to the open window: the text is joined with a single
// space (SPEC 8.6.1), the span end advances, the per-word timing arrays
// concatenate, and seg is recorded as one more exportable cue.
func joinWindow(cur *chunkSegment, seg chunkSegment) {
	cur.Text = cur.Text + " " + seg.Text
	if seg.Span.EndMS > cur.Span.EndMS {
		cur.Span.EndMS = seg.Span.EndMS
	}
	cur.Span.Words = append(cur.Span.Words, seg.Span.Words...)
	cur.Span.Cues = append(cur.Span.Cues, model.CueSpan{
		T: seg.Span.StartMS,
		D: seg.Span.EndMS - seg.Span.StartMS,
		N: utf8.RuneCountInString(seg.Text),
	})
}

// MergeTranscriptChunkWindows is the exported counterpart of
// mergeTranscriptChunkWindows, exposed for tests in the tests/ tree. chunkSec of
// 0 disables merging and returns the input unchanged.
func MergeTranscriptChunkWindows(segs []ChunkSegment, chunkSec, gapSec int) []ChunkSegment {
	in := make([]chunkSegment, 0, len(segs))
	for _, seg := range segs {
		in = append(in, chunkSegment(seg))
	}
	merged := mergeTranscriptChunkWindows(in, transcriptWindow{
		ChunkMS: chunkSec * 1000,
		GapMS:   gapSec * 1000,
	})
	out := make([]ChunkSegment, 0, len(merged))
	for _, seg := range merged {
		out = append(out, ChunkSegment(seg))
	}
	return out
}
