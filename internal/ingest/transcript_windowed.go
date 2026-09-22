package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TranscriptWindow is one decoded audio window: the offset (ms from the start of
// the whole recording) at which the window began, and the window-local decode
// result (its timestamps start at 0 for the window). Exported so the pure merge
// logic can be unit-tested from the tests/ tree (AGENTS.md: no new _test.go under
// internal/).
//
// The same window carries a plain transcription (issue #954) and a Whisper
// translate decode: both produce the `[mm:ss] text` segment format plus optional
// word timings, so one scheduler, one decode loop and one merger serve both.
type TranscriptWindow struct {
	StartMS int
	Res     model.TranscriptResult
}

// TranscriptWindowOverlapMS derives the overlap between consecutive decode windows
// from the window length: enough lookahead that a sentence straddling a boundary
// is fully decoded in the window it starts in, capped so the overlap never
// dominates the window. Deriving it (rather than exposing a second knob) keeps the
// public surface to a single media.translate.whisper_window_sec setting, and gives
// the plain-transcription path (which has no knob at all) the same rule.
func TranscriptWindowOverlapMS(windowMS int) int {
	overlap := windowMS / 5
	if overlap > 10000 {
		overlap = 10000
	}
	if overlap < 0 {
		overlap = 0
	}
	return overlap
}

// MergeTranscriptWindows stitches per-window decode results (each in
// window-local time) into one transcript in absolute time. Each window's segment
// lines and words are offset by the window's start, then de-duplicated against the
// overlap by keeping only those whose absolute start falls in the window's CORE
// [startMS, startMS+stepMS); the final window keeps everything through the end.
// The result is a segment-formatted transcript string (the same `[mm:ss] text`
// shape a single decode returns) plus the flat, time-ordered word list.
//
// It is a pure function of its inputs so the windowing/merge logic is unit-tested
// without a live transcriber.
func MergeTranscriptWindows(windows []TranscriptWindow, stepMS int) (string, []model.TimedWord) {
	if stepMS <= 0 {
		stepMS = 1
	}
	// Collect whole SEGMENTS (a [mm:ss] line plus the words that fall in it) rather
	// than filtering lines and words separately: keeping a segment's text and word
	// timings together lets the overlap de-duplication drop both as a unit.
	var segs []mergedSegment
	for i, w := range windows {
		last := i == len(windows)-1
		// A window's core normally ends where the next scheduled window begins
		// (startMS+stepMS), so its overlap tail is dropped as the next window's
		// core re-decodes it. But `windows` holds only the windows that SURVIVED
		// (translateStructuredWindowed skips a window whose decode failed — silence,
		// music, a transient error), so the next surviving window may start LATER
		// than the scheduled one. In that case nothing re-decodes this window's
		// overlap tail, so extend the core to the actual next surviving window and
		// keep this window's real segments instead of silently dropping them.
		coreEnd := w.StartMS + stepMS
		if !last && windows[i+1].StartMS > coreEnd {
			coreEnd = windows[i+1].StartMS
		}
		for _, s := range windowSegments(w.Res, w.StartMS, i) {
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
	// win identifies the decode window this segment came from, so de-duplication
	// only ever collapses the SAME utterance re-decoded by two overlapping windows,
	// never a speaker who really did repeat themselves inside one window.
	win int
}

// windowSegments splits one window's decode result into absolute-time segments,
// pairing each timestamped line with the words whose (window-local) start falls in
// its span. Timestamps and word starts are offset by the window's start so the
// result is in absolute time. A leading run of un-timestamped text is attached to
// a synthetic segment at the window start.
func windowSegments(res model.TranscriptResult, offsetMS, win int) []mergedSegment {
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
		segs[i] = mergedSegment{startMS: l.start + offsetMS, body: l.body, win: win}
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
// windows. Core-boundary de-duplication (in MergeTranscriptWindows) misses a
// sentence that the two windows time slightly differently, so its start straddles
// the boundary and both copies survive. Here a segment is dropped when it shares
// >= 0.75 of its words with a recent kept segment (within 8 s); the more complete
// wording — and its word timings — is retained, so the transcript never doubles a
// sentence. Only segments from DIFFERENT windows are compared: a repeated phrase
// inside one window is the speaker repeating themselves, not an overlap artifact.
func dedupMergedSegments(segs []mergedSegment) []mergedSegment {
	kept := make([]mergedSegment, 0, len(segs))
	for _, s := range segs {
		dup := false
		for j := len(kept) - 1; j >= 0 && j >= len(kept)-4; j-- {
			if s.startMS-kept[j].startMS > 8000 {
				break
			}
			if kept[j].win == s.win {
				// Same window: whatever it said twice, it really said twice.
				continue
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
			// Count CHARACTERS, not bytes: a Cyrillic or CJK transcript costs 2-3
			// bytes per character, so a byte count would inflate the reading-speed
			// budget and hit the group cap at a third of the intended text, merging
			// non-Latin transcripts far more aggressively than Latin ones.
			chars := utf8.RuneCountInString(cur.body)
			need := int(float64(chars) / mergeTargetCPS * 1000)
			if spanToNext >= need || chars > maxGroupChars {
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

// TranscriptWindowStarts returns the window start offsets (ms) that tile
// [0, totalMS) at stepMS. A trailing window shorter than overlapMS is dropped:
// such a stub is both unreliable to decode (a sub-second Whisper clip yields
// empty/hallucinated text, and avutil.ExtractSegment errors on an empty segment,
// which would fail the whole file) AND redundant — whenever the tail is below the
// overlap the preceding window's extent already reaches totalMS, so as the new
// last window it keeps the tail. Pure so the drop rule is unit-tested.
func TranscriptWindowStarts(totalMS, stepMS, overlapMS int) []int {
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

// DefaultSTTWindowMS is the decode window used when a plain transcription has to
// be windowed (issue #954). Ten minutes keeps one request comfortably inside every
// shipped STT payload cap (self-hosted whisper 50 MB, Mistral 20 MB, OpenAI 25 MB)
// at ordinary speech bitrates, while staying long enough that window boundaries
// remain rare. The request timeout is derived from the window, not fixed, so a
// window that decodes slowly is waited out rather than cut off (issue #962).
const DefaultSTTWindowMS = 10 * 60 * 1000

// minSTTWindowMS floors the window derived from a provider payload cap. A shorter
// window decodes badly (a very short clip makes Whisper hallucinate) and multiplies
// requests, so an extremely dense payload is scheduled at the floor rather than in
// rubble. The floor does not hand the provider an oversized request: extractWithinCap
// measures each cut and halves it (down to minWindowSplitMS) when it overshoots the
// cap, so the cap is enforced on the bytes actually sent, not on an estimate.
const minSTTWindowMS = 30 * 1000

// sttWindowCapHeadroomPct is the share of the provider cap a window may occupy.
// A re-encoded slice is only ROUGHLY proportional to its duration (container
// headers, a variable bitrate, a keyframe-aligned cut), so the window is sized
// against 80% of the cap rather than all of it.
const sttWindowCapHeadroomPct = 80

// STTWindowMS decides the decode window (ms) for one plain transcription: totalMS
// is the recording length, payloadBytes the extracted audio a single request would
// carry, and capBytes the per-request payload cap the provider declares (0 when it
// declares none). It returns 0 when the recording is sent as ONE request, which is
// the pre-#954 behaviour and stays the common case.
//
// Either rule triggers windowing:
//   - the payload exceeds the provider cap. This is the #954 failure: the request
//     is refused outright (WHISPER_FAILED "transcription input too large") and the
//     document lands at status=error, never transcribed.
//   - the recording runs longer than DefaultSTTWindowMS. One request for hours of
//     audio strains the request timeout, and a timeout loses the whole document
//     rather than one window.
//
// When the cap is known the window is shrunk further so a window's share of the
// payload fits inside it with headroom, because a 10-minute window of dense audio
// (uncompressed WAV) can exceed the cap on its own.
//
// Pure, so both trigger rules are unit-tested without a provider.
func STTWindowMS(totalMS, payloadBytes, capBytes int) int {
	if totalMS <= 0 {
		return 0 // duration unknown: there is nothing to schedule windows over
	}
	overCap := capBytes > 0 && payloadBytes > capBytes
	if !overCap && totalMS <= DefaultSTTWindowMS {
		return 0
	}
	windowMS := DefaultSTTWindowMS
	if capBytes > 0 && payloadBytes > 0 {
		budget := int64(capBytes) / 100 * sttWindowCapHeadroomPct
		// Project the default window's share of the payload and shrink the window
		// only when that share does not fit the budget. Comparing first is also what
		// keeps a very large configured cap (media.stt.max_payload_mb clamps to
		// math.MaxInt) from overflowing the multiplication below and collapsing the
		// window to the floor.
		projected := int64(payloadBytes) * int64(DefaultSTTWindowMS) / int64(totalMS)
		if projected > budget {
			windowMS = int(budget * int64(totalMS) / int64(payloadBytes))
		}
	}
	if windowMS < minSTTWindowMS {
		windowMS = minSTTWindowMS
	}
	return windowMS
}

// sttPayloadCapBytes reports the per-request payload cap the transcriber enforces,
// or 0 when it declares none. model.PayloadLimitedTranscriber is an OPTIONAL
// capability: a transcriber that does not implement it counts as uncapped and is
// windowed on the duration rule alone.
func sttPayloadCapBytes(stt model.Transcriber) int {
	limited, ok := stt.(model.PayloadLimitedTranscriber)
	if !ok {
		return 0
	}
	limit := limited.MaxTranscribePayloadBytes()
	if limit < 0 {
		return 0
	}
	return limit
}

// transcribeStructuredWindowed is the payload- and duration-aware wrapper around
// the plain transcription call (issue #954). A recording that fits in one request
// is still sent as one request, byte for byte as before. A recording that does not
// (its audio exceeds the provider's payload cap, or it runs longer than
// DefaultSTTWindowMS) is decoded in overlapping windows and merged back into one
// absolute-time transcript, so a long file yields a transcript instead of a
// document stamped status=error.
//
// The transcript cache key is deliberately unchanged: windowing is DERIVED from
// the media and the provider, not configured, and both paths return the same
// `[mm:ss] text` contract, so a cached transcript stays valid either way.
func (s *Service) transcribeStructuredWindowed(ctx context.Context, relPath string, content []byte) (string, []model.TimedWord, *TranscriptCoverage, error) {
	if len(content) == 0 {
		return withoutCoverage(s.transcribeWith(ctx, s.transcriber, relPath, content))
	}
	capBytes := sttPayloadCapBytes(s.transcriber)
	tmpPath, cleanup, err := stageMediaTemp(content, filepath.Ext(relPath))
	if err != nil {
		// Staging exists only to SLICE the audio; failing it must not lose a
		// document that the single request would have transcribed.
		s.getLogger().Printf("windowed transcription %s: stage failed (%v); sending one request", relPath, err)
		return withoutCoverage(s.transcribeWith(ctx, s.transcriber, relPath, content))
	}
	defer cleanup()

	totalMS := s.probeStagedDurationMS(ctx, tmpPath)
	windowMS := STTWindowMS(totalMS, len(content), capBytes)
	if windowMS <= 0 {
		if capBytes > 0 && len(content) > capBytes {
			// Over the cap, but the duration probe failed (no ffprobe, undecodable
			// container), so there is nothing to schedule windows over. Say so: the
			// provider is about to refuse this request.
			s.getLogger().Printf("windowed transcription %s: payload %d bytes exceeds the provider cap %d bytes but the duration probe failed; sending one request",
				relPath, len(content), capBytes)
		}
		// One request still carries the WHOLE recording, so it gets a timeout sized
		// to the whole recording (issue #962). A nine-minute file is under the
		// windowing threshold, and a hard language decodes it well past 120 s.
		if s.windowScoped() && totalMS > 0 {
			// §8.2.2: a recording that fits one request is ONE window, and the
			// same rules apply to it. Without a duration there is no range to
			// record, so that case keeps the item-scope path below and says so.
			return s.decodeSingleWindowScoped(ctx, relPath, content, totalMS)
		}
		if s.windowScoped() {
			s.getLogger().Printf("windowed transcription %s: media.stt.language_scope=window but the duration probe failed; decoding as one unscoped request", relPath)
		}
		return withoutCoverage(
			s.transcribeWith(ctx, model.TranscriberForAudioDuration(s.transcriber, totalMS), relPath, content))
	}
	text, words, coverage, err := s.decodeWindowedTranscript(ctx, relPath, tmpPath, s.transcriber, totalMS, windowMS, "transcription")
	var cut *windowExtractError
	if errors.As(err, &cut) {
		// ffmpeg is what SLICES the audio. When it is missing, or cannot cut this
		// container, the recording cannot be windowed but it can still be sent
		// whole, exactly as before #954: a slicing failure must not turn a
		// transcript into a failed document. A provider that then refuses the
		// payload reports its own cap honestly.
		s.getLogger().Printf("windowed transcription %s: the audio cannot be sliced (%v); sending one request", relPath, err)
		return withoutCoverage(
			s.transcribeWith(ctx, model.TranscriberForAudioDuration(s.transcriber, totalMS), relPath, content))
	}
	return text, words, coverage, err
}

// decodeSingleWindowScoped runs the §8.2.2 rules over a recording that fits one
// request, treating the whole recording as one window with the whole recording
// as its core. The coverage object is recorded (coverage.languages is required
// under window scope whatever the window count), with one attempted window.
func (s *Service) decodeSingleWindowScoped(ctx context.Context, relPath string, content []byte, totalMS int) (string, []model.TimedWord, *TranscriptCoverage, error) {
	st := s.newWindowLanguageState()
	plan := windowSchedule{totalMS: totalMS, windowMS: totalMS, stepMS: totalMS, capBytes: sttPayloadCapBytes(s.transcriber), label: "transcription"}
	core := CoverageRange{StartMS: 0, EndMS: totalMS}
	wd := s.decodeWindowScoped(ctx, relPath, []windowPiece{{startMS: 0, endMS: totalMS, data: content}}, plan, core, st)
	stats := windowStats{attempted: 1, languages: wd.languages, refused: wd.refused}
	if len(wd.decoded) == 0 {
		if len(wd.refused) > 0 {
			return "", nil, newScopedTranscriptCoverage(stats, totalMS), nil
		}
		return "", nil, nil, wd.err
	}
	stats.decoded = 1
	stats.ranges = wd.covered
	text, words := MergeTranscriptWindows(wd.decoded, totalMS)
	return text, words, newScopedTranscriptCoverage(stats, totalMS), nil
}

// withoutCoverage adapts a SINGLE-request decode to the windowed decode's return
// shape. A decode that took one request records no §8.6.13 coverage: per §5.2
// absence means "no assertion", and claiming coverage for a decode that was never
// windowed would make the field meaningless for the decodes that were.
func withoutCoverage(text string, words []model.TimedWord, err error) (string, []model.TimedWord, *TranscriptCoverage, error) {
	return text, words, nil, err
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
	_, structured := s.translateSTT.(model.StructuredTranscriber)
	if windowMS <= 0 || !structured || len(content) == 0 {
		return s.translateStructured(ctx, doc, content)
	}
	tmpPath, cleanup, err := stageMediaTemp(content, filepath.Ext(doc.RelPath))
	if err != nil {
		s.getLogger().Printf("windowed translate %s: stage failed (%v); decoding in one pass", doc.RelPath, err)
		return s.translateStructured(ctx, doc, content)
	}
	defer cleanup()

	// A recording that fits in a single window would be decoded once anyway, so skip
	// the slicing and the duplicate calls (timestamp drift accumulates over LENGTH,
	// so a sub-window recording has nothing to correct). A failed duration probe
	// lands here too and degrades to the single pass, because a missing ffprobe must
	// not fail a document that one request can still translate.
	totalMS := s.probeStagedDurationMS(ctx, tmpPath)
	if totalMS <= windowMS {
		// translateStructured with the request sized to the whole recording (#962).
		return s.transcribeWith(ctx, model.TranscriberForAudioDuration(s.translateSTT, totalMS), doc.RelPath, content)
	}
	// The translate pass re-decodes the SOURCE audio into a second, derived
	// transcript; §8.6.13 scopes the recorded coverage to the authoritative source
	// transcript, so the coverage is dropped here and the decode is otherwise
	// unchanged (issue #961).
	text, words, _, err := s.decodeWindowedTranscript(ctx, doc.RelPath, tmpPath, s.translateSTT, totalMS, windowMS, "translate")
	var cut *windowExtractError
	if errors.As(err, &cut) {
		s.getLogger().Printf("windowed translate %s: the audio cannot be sliced (%v); decoding in one pass", doc.RelPath, err)
		return s.translateStructured(ctx, doc, content)
	}
	return text, words, err
}

// decodeWindowedTranscript decodes the staged audio at tmpPath in overlapping
// windows through stt and merges them into one absolute-time transcript. label
// names the operation in logs ("transcription" or "translate"); it is the only
// difference between the two callers, because a translate decode returns the same
// segment/word shape a transcription does.
func (s *Service) decodeWindowedTranscript(ctx context.Context, relPath, tmpPath string, stt model.Transcriber, totalMS, windowMS int, label string) (string, []model.TimedWord, *TranscriptCoverage, error) {
	overlapMS := TranscriptWindowOverlapMS(windowMS)
	stepMS := windowMS - overlapMS
	if stepMS <= 0 {
		stepMS = windowMS
	}
	// §8.2.2: under window scope the transcription path carries a language state
	// across windows; the translate path (a fixed English target) never does.
	var st *windowLanguageState
	if label == "transcription" && s.windowScoped() {
		st = s.newWindowLanguageState()
	}
	windows, stats, err := s.decodeTranscriptWindows(ctx, relPath, tmpPath, stt, windowSchedule{
		totalMS:  totalMS,
		windowMS: windowMS,
		stepMS:   stepMS,
		capBytes: sttPayloadCapBytes(stt),
		label:    label,
	}, st)
	if err != nil {
		return "", nil, nil, err
	}
	// One progress line per document: how much of the recording actually decoded.
	s.getLogger().Printf("windowed %s %s: %d/%d windows decoded (window %ds, overlap %ds, duration %ds)",
		label, relPath, stats.decoded, stats.attempted, windowMS/1000, overlapMS/1000, totalMS/1000)
	text, words := MergeTranscriptWindows(windows, stepMS)
	// §8.6.13: the counts and the decoded ranges leave this function instead of
	// dying in the log line above, so the transcript representation can record what
	// it does and does not cover.
	if st != nil {
		return text, words, newScopedTranscriptCoverage(stats, totalMS), nil
	}
	return text, words, newTranscriptCoverage(stats.attempted, stats.decoded, totalMS, stats.ranges), nil
}

// windowSchedule is the plan for one windowed decode: the recording length, the
// window and its step (window minus overlap), the provider's payload cap (0 when
// it declares none), and the log label.
type windowSchedule struct {
	totalMS  int
	windowMS int
	stepMS   int
	capBytes int
	label    string
}

// windowStats counts the scheduled windows that were attempted and the ones that
// yielded at least one decoded piece, and records WHICH stretches of the
// recording those pieces covered (SPEC §8.6.13). The ranges are raw and may
// overlap (consecutive windows overlap by design); newTranscriptCoverage
// coalesces them.
type windowStats struct {
	attempted int
	decoded   int
	ranges    []CoverageRange
	// languages and refused are the §8.2.2 per-window records, populated only
	// under media.stt.language_scope: window (a nil state leaves them empty).
	languages []model.CoverageLanguage
	refused   []model.RefusedRange
}

// decodeTranscriptWindows extracts and decodes each scheduled window from the
// staged audio at tmpPath, returning the pieces that decoded to content (in
// schedule order) and the per-window counts. It prefers the structured
// (word-timing) capability per piece and degrades to text-only, exactly as a
// single-request decode does.
//
// A window that fails — the cut fails, or the decode fails on silence, music or a
// provider error that survived the client's retries — is skipped with a warning
// rather than aborting the whole recording, because a long recording routinely has
// stretches with nothing to transcribe and one bad region must not cost the other
// two hours. If EVERY attempted window fails that is systemic (ffmpeg absent,
// provider down, bad credentials, unsupported media), so it is returned as an
// error and the document fails exactly as it does today.
//
// st is the §8.2.2 per-window language state, nil under item scope. With a
// state, each window is decoded by decodeWindowScoped instead: identified,
// routed, floor-checked and recorded. A window REFUSED there is neither decoded
// nor failed: it is counted in attempted, listed in refused, and never turns the
// "all windows failed" verdict below into an error, because a refusal is a
// decision, not a fault.
func (s *Service) decodeTranscriptWindows(ctx context.Context, relPath, tmpPath string, stt model.Transcriber, plan windowSchedule, st *windowLanguageState) ([]TranscriptWindow, windowStats, error) {
	var windows []TranscriptWindow
	var firstDecodeErr, firstCutErr error
	var stats windowStats
	starts := TranscriptWindowStarts(plan.totalMS, plan.stepMS, TranscriptWindowOverlapMS(plan.windowMS))
	for i := range starts {
		stats.attempted++
		wd, cutErr := s.decodeScheduledWindow(ctx, relPath, tmpPath, stt, plan, starts, i, st)
		if cutErr != nil {
			if firstCutErr == nil {
				firstCutErr = cutErr
			}
			continue
		}
		stats.languages = append(stats.languages, wd.languages...)
		stats.refused = append(stats.refused, wd.refused...)
		if wd.err != nil && firstDecodeErr == nil {
			firstDecodeErr = wd.err
		}
		if len(wd.decoded) == 0 {
			continue
		}
		stats.decoded++
		stats.ranges = append(stats.ranges, wd.covered...)
		windows = append(windows, wd.decoded...)
	}
	if stats.attempted > 0 && stats.decoded == 0 && len(stats.refused) > 0 {
		// Every window was refused, or refused and failed in some mix. Nothing
		// decoded, but the refusals are the caller's to judge (§8.2.2 terminal
		// status), not a provider fault to retry: return the empty result with its
		// record rather than an error.
		return nil, stats, nil
	}
	if stats.attempted > 0 && stats.decoded == 0 {
		// Prefer the provider's failure over a cut failure: it is the one whose
		// retryable/terminal classification decides whether the document stays
		// pending, and it must survive the aggregation rather than be flattened into
		// an opaque string. With no decode failure recorded, nothing could be cut,
		// and the windowExtractError tells the caller to send one request instead.
		cause := firstDecodeErr
		if cause == nil {
			cause = firstCutErr
		}
		return nil, stats, fmt.Errorf("windowed %s %s: all %d windows failed: %w", plan.label, relPath, stats.attempted, cause)
	}
	return windows, stats, nil
}

// decodeScheduledWindow cuts and decodes the i-th scheduled window. A cut
// failure is returned as the windowExtractError the caller aggregates; a decode
// outcome (including a scoped refusal) comes back in the windowDecode. Under
// window scope (st != nil) the window goes through decodeWindowScoped with its
// core, the stretch the merge keeps of it: its start through the next window's
// start, or the recording's end for the last window.
func (s *Service) decodeScheduledWindow(ctx context.Context, relPath, tmpPath string, stt model.Transcriber, plan windowSchedule, starts []int, i int, st *windowLanguageState) (windowDecode, *windowExtractError) {
	start := starts[i]
	end := start + plan.windowMS
	if end > plan.totalMS {
		end = plan.totalMS
	}
	pieces, err := s.extractWithinCap(ctx, tmpPath, start, end, plan.capBytes, maxWindowSplits)
	if err != nil {
		s.getLogger().Printf("windowed %s: cannot cut window [%d,%d]ms of %s: %v", plan.label, start, end, relPath, err)
		return windowDecode{}, &windowExtractError{fmt.Errorf("extract %s window [%d,%d]ms: %w", plan.label, start, end, err)}
	}
	if st != nil {
		core := CoverageRange{StartMS: start, EndMS: end}
		if i+1 < len(starts) && starts[i+1] < core.EndMS {
			core.EndMS = starts[i+1]
		}
		return s.decodeWindowScoped(ctx, relPath, pieces, plan, core, st), nil
	}
	decoded, covered, decodeErr := s.decodeWindowPieces(ctx, relPath, stt, pieces, plan)
	return windowDecode{decoded: decoded, covered: covered, err: decodeErr}, nil
}

// decodeWindowPieces decodes every piece of one scheduled window and returns the
// pieces that produced content plus the first decode failure. A piece that fails is
// skipped, so a window split by the payload cap keeps the halves that did decode.
//
// A piece that is STILL over the provider cap when the split budget runs out is not
// sent at all: the client enforces the same cap locally, so the request would be
// refused without reaching the server. Skipping it costs the same audio and says
// exactly why, and when every window ends this way the document fails with that
// reason instead of a generic refusal.
func (s *Service) decodeWindowPieces(ctx context.Context, relPath string, stt model.Transcriber, pieces []windowPiece, plan windowSchedule) ([]TranscriptWindow, []CoverageRange, error) {
	var out []TranscriptWindow
	var covered []CoverageRange
	var firstErr error
	for _, p := range pieces {
		if plan.capBytes > 0 && len(p.data) > plan.capBytes {
			err := fmt.Errorf("%s window [%d,%d]ms is %d bytes after %d splits, over the provider cap of %d bytes",
				plan.label, p.startMS, p.endMS, len(p.data), maxWindowSplits, plan.capBytes)
			if firstErr == nil {
				firstErr = err
			}
			s.getLogger().Printf("windowed %s: skip window of %s: %v (raise the provider payload cap or re-encode the media)", plan.label, relPath, err)
			continue
		}
		// Size the request to the audio this piece carries, so a window that decodes
		// slowly (a hard language, a busy GPU, CPU inference) is waited out instead
		// of cut off at a constant and left as a hole in the transcript (#962).
		text, words, err := s.transcribeWith(ctx, model.TranscriberForAudioDuration(stt, p.endMS-p.startMS), relPath, p.data)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.getLogger().Printf("windowed %s: skip window [%d,%d]ms of %s: %v", plan.label, p.startMS, p.endMS, relPath, err)
			continue
		}
		out = append(out, TranscriptWindow{StartMS: p.startMS, Res: model.TranscriptResult{Text: text, Words: words}})
		// The piece's whole span counts as covered, not the span its segments
		// happen to fill: a decoded window with a silent tail was still LISTENED to,
		// and reporting the silence as an uncovered gap would misread "nothing was
		// said" as "nothing was transcribed", which is the exact confusion §8.6.13
		// exists to remove.
		covered = append(covered, CoverageRange{StartMS: p.startMS, EndMS: p.endMS})
	}
	return out, covered, firstErr
}

// windowPiece is one slice of staged audio ready to send: the absolute range it
// covers and its extracted bytes.
type windowPiece struct {
	startMS int
	endMS   int
	data    []byte
}

// maxWindowSplits bounds how often an oversized window is halved before it is sent
// anyway. Three halvings take a window to an eighth of its length, which is far
// past any plausible bitrate surprise; splitting further would produce clips too
// short to decode reliably.
const maxWindowSplits = 3

// minWindowSplitMS stops the halving at a length Whisper can still decode. A clip
// below this yields empty or hallucinated text, so an even smaller request is not
// an improvement over letting the provider report its cap.
const minWindowSplitMS = 10 * 1000

// extractWithinCap cuts [startMS,endMS) from the staged media and, when the
// extracted bytes overshoot the provider cap, halves the range and cuts again.
// STTWindowMS sizes a window from the WHOLE file's bytes-per-millisecond, while
// avutil.ExtractSegment copies the source codec, so a variable-bitrate stretch or a
// keyframe-aligned cut can still come out larger than the estimate. A piece that is
// still over the cap when the split budget runs out is returned anyway, and
// decodeWindowPieces then skips it with a precise reason rather than handing the
// provider a request its own client would refuse; the rest of the recording still
// decodes.
func (s *Service) extractWithinCap(ctx context.Context, path string, startMS, endMS, capBytes, depth int) ([]windowPiece, error) {
	data, err := s.extractMediaSegment(ctx, path, startMS, endMS)
	if err != nil {
		return nil, err
	}
	if capBytes <= 0 || len(data) <= capBytes || depth <= 0 || endMS-startMS <= minWindowSplitMS {
		return []windowPiece{{startMS: startMS, endMS: endMS, data: data}}, nil
	}
	mid := startMS + (endMS-startMS)/2
	head, err := s.extractWithinCap(ctx, path, startMS, mid, capBytes, depth-1)
	if err != nil {
		return nil, err
	}
	tail, err := s.extractWithinCap(ctx, path, mid, endMS, capBytes, depth-1)
	if err != nil {
		return nil, err
	}
	return append(head, tail...), nil
}

// windowExtractError marks a failure to SLICE the staged audio (ffmpeg absent, an
// unsupported container, a seek that ffmpeg refuses). It is distinct from a decode
// failure, because the caller answers it differently: a recording that cannot be
// cut is still sent as one request, while a decode failure is the provider's and
// is reported as such.
type windowExtractError struct{ err error }

func (e *windowExtractError) Error() string { return e.err.Error() }
func (e *windowExtractError) Unwrap() error { return e.err }

// stageMediaTemp writes in-memory media to a temp file that keeps the original
// extension, because avutil probes and slices by PATH. The returned cleanup
// removes the file and is always safe to call.
func stageMediaTemp(content []byte, ext string) (string, func(), error) {
	noop := func() {}
	// CreateTemp treats "*" as the random-string placeholder and rejects path
	// separators, so an odd extension is dropped rather than corrupting the pattern.
	if strings.ContainsAny(ext, `*/\`) {
		ext = ""
	}
	tmp, err := os.CreateTemp("", "dir2mcp-window-*"+ext)
	if err != nil {
		return "", noop, fmt.Errorf("stage audio for windowed decode: %w", err)
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", noop, fmt.Errorf("write staged audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("flush staged audio: %w", err)
	}
	return path, cleanup, nil
}

// probeStagedDurationMS probes the staged media's duration through
// ProbeDurationFunc (default avutil.Duration, ffprobe). It reports 0 instead of an
// error when the probe fails or the binary is absent: the caller then keeps the
// single-request path, so a missing ffprobe never costs a transcript.
func (s *Service) probeStagedDurationMS(ctx context.Context, path string) int {
	probe := s.ProbeDurationFunc
	if probe == nil {
		probe = avutil.Duration
	}
	dur, err := probe(ctx, path)
	if err != nil || dur <= 0 {
		return 0
	}
	return int(dur.Milliseconds())
}

// extractMediaSegment slices [startMS,endMS) out of the staged media through
// ExtractSegmentFunc (default avutil.ExtractSegment, ffmpeg).
func (s *Service) extractMediaSegment(ctx context.Context, path string, startMS, endMS int) ([]byte, error) {
	extract := s.ExtractSegmentFunc
	if extract == nil {
		extract = avutil.ExtractSegment
	}
	return extract(ctx, path, startMS, endMS)
}
