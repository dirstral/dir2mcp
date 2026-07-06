package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TranslateWindow is one decoded audio window: the offset (ms from the start of
// the whole recording) at which the window began, and the window-local translate
// result (its timestamps start at 0 for the window). Exported so the pure merge
// logic can be unit-tested from the tests/ tree (AGENTS.md: no new _test.go under
// internal/).
type TranslateWindow struct {
	StartMS int
	Res     model.TranscriptResult
}

// WhisperTranslateOverlapMS derives the overlap between consecutive decode windows
// from the window length: enough lookahead that a sentence straddling a boundary
// is fully decoded in the window it starts in, capped so the overlap never
// dominates the window. Deriving it (rather than exposing a second knob) keeps the
// public surface to a single media.translate.whisper_window_sec setting.
func WhisperTranslateOverlapMS(windowMS int) int {
	overlap := windowMS / 5
	if overlap > 10000 {
		overlap = 10000
	}
	if overlap < 0 {
		overlap = 0
	}
	return overlap
}

// MergeTranslateWindows stitches per-window translate results (each in
// window-local time) into one transcript in absolute time. Each window's segment
// lines and words are offset by the window's start, then de-duplicated against the
// overlap by keeping only those whose absolute start falls in the window's CORE
// [startMS, startMS+stepMS); the final window keeps everything through the end.
// The result is a segment-formatted transcript string (the same `[mm:ss] text`
// shape a single decode returns) plus the flat, time-ordered word list.
//
// It is a pure function of its inputs so the windowing/merge logic is unit-tested
// without a live transcriber.
func MergeTranslateWindows(windows []TranslateWindow, stepMS int) (string, []model.TimedWord) {
	if stepMS <= 0 {
		stepMS = 1
	}
	// Collect whole SEGMENTS (a [mm:ss] line plus the words that fall in it) rather
	// than filtering lines and words separately: keeping a segment's text and word
	// timings together lets the overlap de-duplication drop both as a unit.
	var segs []mergedSegment
	for i, w := range windows {
		coreEnd := w.StartMS + stepMS
		last := i == len(windows)-1
		for _, s := range windowSegments(w.Res, w.StartMS) {
			if s.startMS < w.StartMS {
				s.startMS = w.StartMS
			}
			if !last && s.startMS >= coreEnd {
				continue
			}
			segs = append(segs, s)
		}
	}
	sort.SliceStable(segs, func(a, b int) bool { return segs[a].startMS < segs[b].startMS })
	segs = dedupMergedSegments(segs)
	segs = groupMistimedSegments(segs)

	var lines []string
	var words []model.TimedWord
	for _, s := range segs {
		if strings.TrimSpace(s.body) == "" {
			continue
		}
		lines = append(lines, formatTimestampMarker(s.startMS)+" "+s.body)
		words = append(words, s.words...)
	}
	sort.SliceStable(words, func(a, b int) bool { return words[a].StartMS < words[b].StartMS })
	return strings.Join(lines, "\n"), words
}

// mergedSegment is one transcript segment in absolute time: its [mm:ss] start, the
// spoken text, and the per-word timings that fall within it. Carrying words with
// their segment lets de-duplication move text and timing together.
type mergedSegment struct {
	startMS int
	body    string
	words   []model.TimedWord
}

// windowSegments splits one window's translate result into absolute-time segments,
// pairing each timestamped line with the words whose (window-local) start falls in
// its span. Timestamps and word starts are offset by the window's start so the
// result is in absolute time. A leading run of un-timestamped text is attached to
// a synthetic segment at the window start.
func windowSegments(res model.TranscriptResult, offsetMS int) []mergedSegment {
	type lineT struct {
		start int
		body  string
	}
	var lns []lineT
	for _, raw := range strings.Split(res.Text, "\n") {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		st, body, ok := parseTranscriptTimestamp(t)
		if !ok {
			if len(lns) > 0 {
				lns[len(lns)-1].body = strings.TrimSpace(lns[len(lns)-1].body + " " + t)
			} else {
				lns = append(lns, lineT{start: 0, body: t})
			}
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		lns = append(lns, lineT{start: st, body: body})
	}
	if len(lns) == 0 {
		return nil
	}
	segs := make([]mergedSegment, len(lns))
	for i, l := range lns {
		segs[i] = mergedSegment{startMS: l.start + offsetMS, body: l.body}
	}
	// Assign each word to the last segment whose local start is <= the word's local
	// start (segments are in playback order).
	for _, wd := range res.Words {
		idx := 0
		for i := range lns {
			if lns[i].start <= wd.StartMS {
				idx = i
			} else {
				break
			}
		}
		segs[idx].words = append(segs[idx].words, model.TimedWord{
			Word: wd.Word, StartMS: wd.StartMS + offsetMS, EndMS: wd.EndMS + offsetMS,
		})
	}
	return segs
}

// dedupMergedSegments removes near-duplicate segments left by overlapping decode
// windows. Core-boundary de-duplication (in MergeTranslateWindows) misses a
// sentence that the two windows time slightly differently, so its start straddles
// the boundary and both copies survive. Here a segment is dropped when it shares
// >= 0.75 of its words with a recent kept segment (within 8 s); the more complete
// wording — and its word timings — is retained, so the transcript never doubles a
// sentence.
func dedupMergedSegments(segs []mergedSegment) []mergedSegment {
	kept := make([]mergedSegment, 0, len(segs))
	for _, s := range segs {
		dup := false
		for j := len(kept) - 1; j >= 0 && j >= len(kept)-4; j-- {
			if s.startMS-kept[j].startMS > 8000 {
				break
			}
			if segmentWordOverlap(kept[j].body, s.body) >= 0.75 {
				if len(s.body) > len(kept[j].body) { // keep the fuller decode + its words
					kept[j].body = s.body
					kept[j].words = s.words
				}
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, s)
		}
	}
	return kept
}

// segmentWordOverlap is the fraction of the smaller segment's distinct words that
// also appear in the other, comparing case- and punctuation-insensitively.
func segmentWordOverlap(a, b string) float64 {
	sa, sb := segmentWordSet(a), segmentWordSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	common := 0
	for w := range sa {
		if sb[w] {
			common++
		}
	}
	den := len(sa)
	if len(sb) < den {
		den = len(sb)
	}
	return float64(common) / float64(den)
}

// mergeTargetCPS is the reading speed (characters per second) below which a
// segment's display span is considered adequate for its text; segments timed
// tighter than this are merged by groupMistimedSegments.
const mergeTargetCPS = 17.0

// groupMistimedSegments merges consecutive segments whose display span — the gap
// until the next segment starts — is too short to read the accumulated text at
// mergeTargetCPS. These are window-boundary timing artifacts: a segment carries a
// couple of seconds of speech but the next segment's start lands implausibly
// close, so a segment-timed export (reflow) would crush it into an unreadable
// sub-second cue. Merging gives the combined text the combined span, so reflow
// then splits it into legible, comfortably-paced cues. A genuinely long dense run
// still breaks into groups once the accumulated text passes a cap, and the group
// keeps the FIRST (spoken) start as its anchor so timing is preserved.
func groupMistimedSegments(segs []mergedSegment) []mergedSegment {
	const maxGroupChars = 300
	out := make([]mergedSegment, 0, len(segs))
	for i := 0; i < len(segs); {
		cur := segs[i]
		j := i + 1
		for j < len(segs) {
			spanToNext := segs[j].startMS - cur.startMS
			need := int(float64(len(cur.body)) / mergeTargetCPS * 1000)
			if spanToNext >= need || len(cur.body) > maxGroupChars {
				break
			}
			cur.body = strings.TrimSpace(cur.body + " " + segs[j].body)
			cur.words = append(cur.words, segs[j].words...)
			j++
		}
		out = append(out, cur)
		i = j
	}
	return out
}

func segmentWordSet(s string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,!?;:\"'()[]…»«")
		if w != "" {
			m[w] = true
		}
	}
	return m
}

// TranslateWindowStarts returns the window start offsets (ms) that tile
// [0, totalMS) at stepMS. A trailing window shorter than overlapMS is dropped:
// such a stub is both unreliable to decode (a sub-second Whisper clip yields
// empty/hallucinated text, and avutil.ExtractSegment errors on an empty segment,
// which would fail the whole file) AND redundant — whenever the tail is below the
// overlap the preceding window's extent already reaches totalMS, so as the new
// last window it keeps the tail. Pure so the drop rule is unit-tested.
func TranslateWindowStarts(totalMS, stepMS, overlapMS int) []int {
	if stepMS <= 0 {
		stepMS = 1
	}
	var starts []int
	for start := 0; start < totalMS; start += stepMS {
		starts = append(starts, start)
	}
	if n := len(starts); n >= 2 && totalMS-starts[n-1] < overlapMS {
		starts = starts[:n-1]
	}
	return starts
}

// translateStructuredWindowed is the media.translate.whisper_window_sec-aware
// wrapper around translateStructured. With no window configured (<= 0) it is a
// straight pass-through, so existing corpora are unchanged. With a window it
// decodes the audio in overlapping windows via Whisper's translate task and
// merges them, so timestamp drift cannot accumulate across a long recording.
// Windowing requires the structured (word-timing) transcriber; when the
// transcriber is text-only it transparently falls back to the single-pass decode.
func (s *Service) translateStructuredWindowed(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	windowMS := s.cfg.MediaTranslateWhisperWindowSec * 1000
	st, ok := s.translateSTT.(model.StructuredTranscriber)
	if windowMS <= 0 || !ok {
		return s.translateStructured(ctx, doc, content)
	}
	stepMS := windowMS - WhisperTranslateOverlapMS(windowMS)
	if stepMS <= 0 {
		stepMS = windowMS
	}

	// avutil slices by file path; the audio arrives as bytes, so stage a temp file.
	tmp, err := os.CreateTemp("", "dir2mcp-xlate-*"+filepath.Ext(doc.RelPath))
	if err != nil {
		return "", nil, fmt.Errorf("stage audio for windowed translate: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", nil, fmt.Errorf("write staged audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", nil, fmt.Errorf("flush staged audio: %w", err)
	}

	dur, err := avutil.Duration(ctx, tmpPath)
	if err != nil {
		return "", nil, fmt.Errorf("probe audio duration for windowed translate: %w", err)
	}
	totalMS := int(dur.Milliseconds())
	if totalMS <= 0 {
		return s.translateStructured(ctx, doc, content)
	}

	var windows []TranslateWindow
	attempted, failed := 0, 0
	for _, start := range TranslateWindowStarts(totalMS, stepMS, WhisperTranslateOverlapMS(windowMS)) {
		end := start + windowMS
		if end > totalMS {
			end = totalMS
		}
		seg, err := avutil.ExtractSegment(ctx, tmpPath, start, end)
		if err != nil {
			return "", nil, fmt.Errorf("extract translate window [%d,%d]ms: %w", start, end, err)
		}
		attempted++
		res, err := st.TranscribeStructured(ctx, doc.RelPath, seg)
		if err != nil {
			// A window over silence or music legitimately decodes to nothing (the
			// provider reports "no text content"); skip it rather than abort the
			// whole recording — a long interview routinely has silent stretches and
			// a silent tail. A systemic failure (provider down) still surfaces below
			// because every window fails.
			failed++
			s.getLogger().Printf("windowed translate: skip window [%d,%d]ms: %v", start, end, err)
			continue
		}
		windows = append(windows, TranslateWindow{StartMS: start, Res: res})
	}
	if attempted > 0 && failed == attempted {
		return "", nil, fmt.Errorf("windowed translate %s: all %d windows failed", doc.RelPath, failed)
	}

	text, words := MergeTranslateWindows(windows, stepMS)
	return text, words, nil
}
