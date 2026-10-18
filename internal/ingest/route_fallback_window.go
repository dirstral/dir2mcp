package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// This file is the window-scope half of the SPEC §8.2.4 retry (spec 0.82.0,
// dir2mcp #1122). Only the decode windows inside a coverage.languages entry
// that records fallback_from are decoded again. Their text is spliced into the
// cached merged transcript in place (§8.6.13): the segments inside the
// window's core are replaced, and every other window keeps its cached text and
// its entries.

// fallbackWindow is one scheduled decode window that holds fallback text: its
// extent, its core (the stretch the merge keeps of it), whether it is the last
// window, and the fallback entry inside it.
type fallbackWindow struct {
	startMS, endMS int
	core           CoverageRange
	last           bool
	entry          model.CoverageLanguage
}

// windowRetryResult is the retry of one fallback window: the pieces a route
// decoded (nil when the retry failed), the route that decoded them, the
// candidate a new default profile replaced (set only then), and the failure.
type windowRetryResult struct {
	win          fallbackWindow
	results      []pieceResult
	route        routedSTT
	fallbackFrom string
	err          error
}

// windowRetryPlan is the decode schedule rebuilt for a retry: the plan, the
// window starts, and the staged audio the windows are cut from.
type windowRetryPlan struct {
	plan    windowSchedule
	starts  []int
	tmpPath string
	audio   []byte
	relPath string
}

// retryWindowFallback decodes each fallback window of a window-scoped
// transcript once more on its route and splices the result into the cached
// transcript (SPEC §8.2.4).
func (s *Service) retryWindowFallback(ctx context.Context, doc model.Document, content []byte, hit cachedTranscript, v fallbackVerdict) fallbackRetry {
	var outcomes []windowRetryResult
	relPath, audio, ok, err := s.transcriptInput(ctx, doc, content)
	if err == nil && !ok {
		err = fmt.Errorf("no audio track to decode")
	}
	if err == nil {
		tmpPath, cleanup, serr := stageMediaTemp(audio, filepath.Ext(relPath))
		err = serr
		if serr == nil {
			defer cleanup()
			rp := s.rebuildWindowPlan(ctx, tmpPath, audio, relPath, hit.coverage)
			wins := fallbackWindows(rp, hit.coverage)
			if len(wins) == 0 {
				// The rebuilt schedule does not match the cached record (another
				// duration or payload cap): no window can be retried in place, so
				// the retry is a failure, never a silent recovery.
				err = fmt.Errorf("the rebuilt decode schedule holds no fallback window")
			}
			for _, w := range wins {
				outcomes = append(outcomes, s.retryOneWindow(ctx, rp, w, v))
			}
		}
	}
	if err != nil {
		// Nothing could be decoded: every fallback window failed its retry.
		for _, w := range fallbackWindowsByEntry(hit.coverage) {
			outcomes = append(outcomes, windowRetryResult{win: w, err: err})
		}
	}
	return s.settleWindowRetry(doc, hit, outcomes, v)
}

// rebuildWindowPlan rebuilds the decode schedule the windowed decode used, so
// a retried window has the same extent and core it had then (SPEC §8.6.13).
// The duration comes from the probe, else from the cached coverage.
func (s *Service) rebuildWindowPlan(ctx context.Context, tmpPath string, audio []byte, relPath string, coverage *TranscriptCoverage) windowRetryPlan {
	totalMS := s.probeStagedDurationMS(ctx, tmpPath)
	if totalMS <= 0 && coverage != nil {
		totalMS = coverage.DurationMS
	}
	capBytes := sttPayloadCapBytes(s.transcriber)
	rp := windowRetryPlan{tmpPath: tmpPath, audio: audio, relPath: relPath}
	windowMS := STTWindowMS(totalMS, len(audio), capBytes)
	if windowMS <= 0 {
		// The recording fit one request: one window, the whole recording.
		rp.plan = windowSchedule{totalMS: totalMS, windowMS: totalMS, stepMS: totalMS, capBytes: capBytes, label: "transcription"}
		rp.starts = []int{0}
		return rp
	}
	overlapMS := TranscriptWindowOverlapMS(windowMS)
	stepMS := windowMS - overlapMS
	if stepMS <= 0 {
		stepMS = windowMS
	}
	rp.plan = windowSchedule{totalMS: totalMS, windowMS: windowMS, stepMS: stepMS, capBytes: capBytes, label: "transcription"}
	rp.starts = TranscriptWindowStarts(totalMS, stepMS, overlapMS)
	return rp
}

// fallbackWindows lists the scheduled windows whose core overlaps a
// coverage.languages entry that records fallback_from, in schedule order.
func fallbackWindows(rp windowRetryPlan, coverage *TranscriptCoverage) []fallbackWindow {
	var out []fallbackWindow
	for i, start := range rp.starts {
		end := start + rp.plan.windowMS
		if end > rp.plan.totalMS {
			end = rp.plan.totalMS
		}
		core := CoverageRange{StartMS: start, EndMS: end}
		if i+1 < len(rp.starts) && rp.starts[i+1] < core.EndMS {
			core.EndMS = rp.starts[i+1]
		}
		if e, ok := fallbackEntryIn(coverage, core); ok {
			out = append(out, fallbackWindow{startMS: start, endMS: end, core: core, last: i == len(rp.starts)-1, entry: e})
		}
	}
	return out
}

// fallbackWindowsByEntry is the fallback windows named by the entries alone,
// for a retry that could not rebuild the schedule (the audio could not be
// staged): each fallback entry's range is treated as one window.
func fallbackWindowsByEntry(coverage *TranscriptCoverage) []fallbackWindow {
	var out []fallbackWindow
	for _, e := range coverageLanguages(coverage) {
		if e.FallbackFrom != "" {
			out = append(out, fallbackWindow{startMS: e.StartMS, endMS: e.EndMS, core: CoverageRange{StartMS: e.StartMS, EndMS: e.EndMS}, entry: e})
		}
	}
	return out
}

// fallbackEntryIn returns the first fallback entry that overlaps core.
func fallbackEntryIn(coverage *TranscriptCoverage, core CoverageRange) (model.CoverageLanguage, bool) {
	for _, e := range coverageLanguages(coverage) {
		if e.FallbackFrom != "" && e.StartMS < core.EndMS && e.EndMS > core.StartMS {
			return e, true
		}
	}
	return model.CoverageLanguage{}, false
}

// retryOneWindow decodes one fallback window on its route. A retry succeeds
// only when every piece decodes and the per-window floor and quality gate keep
// the text, so the window's coverage range is unchanged. When the route fails
// and the producer changed under on_route_error: default, the current default
// profile decodes the window once instead (SPEC §8.2.4).
func (s *Service) retryOneWindow(ctx context.Context, rp windowRetryPlan, w fallbackWindow, v fallbackVerdict) windowRetryResult {
	pieces, err := s.retryPieces(ctx, rp, w)
	if err != nil {
		return windowRetryResult{win: w, err: err}
	}
	lang := w.entry.Language
	cand, err := s.retryCandidate(lang, w.entry.FallbackFrom)
	if err == nil {
		results, derr := s.decodePieces(ctx, rp.relPath, cand.stt, pieces, rp.plan)
		err = s.retryDecodeError(results, pieces, derr, cand, lang, w.core)
		if err == nil {
			return windowRetryResult{win: w, results: results, route: cand}
		}
	}
	if !v.producerChanged || !s.routeErrorFallback {
		return windowRetryResult{win: w, err: err}
	}
	failed := cand.route
	if failed == "" {
		failed = w.entry.FallbackFrom
	}
	def := s.defaultRoute()
	results, derr := s.decodePieces(ctx, rp.relPath, def.stt, pieces, rp.plan)
	// The default's text gets the same per-window floor and quality gate as a
	// first decode's fallback (decodeWindowScoped judges it too).
	if derr = s.retryDecodeError(results, pieces, derr, def, lang, w.core); derr != nil {
		return windowRetryResult{win: w, err: fmt.Errorf("route %q: %w; default profile %q: %v", failed, err, def.route, derr)}
	}
	return windowRetryResult{win: w, results: results, route: def, fallbackFrom: failed}
}

// retryPieces cuts one fallback window from the staged audio, within the
// provider cap. A one-window schedule sends the whole recording, as the
// original decode did.
func (s *Service) retryPieces(ctx context.Context, rp windowRetryPlan, w fallbackWindow) ([]windowPiece, error) {
	if len(rp.starts) == 1 {
		return []windowPiece{{startMS: 0, endMS: rp.plan.totalMS, data: rp.audio}}, nil
	}
	return s.extractWithinCap(ctx, rp.tmpPath, w.startMS, w.endMS, rp.plan.capBytes, maxWindowSplits)
}

// retryDecodeError judges a retried window: nil when every piece decoded and
// the per-window floor and quality gate keep the text.
func (s *Service) retryDecodeError(results []pieceResult, pieces []windowPiece, derr error, route routedSTT, lang string, core CoverageRange) error {
	if derr != nil {
		return derr
	}
	if len(results) != len(pieces) {
		return fmt.Errorf("route %q decoded %d of %d pieces", route.route, len(results), len(pieces))
	}
	if v := s.judgeWindow(results, route, lang, core); v.reason != "" {
		return fmt.Errorf("route %q output refused (%s)", route.route, v.reason)
	}
	return nil
}

// settleWindowRetry builds the transcript the index gets and the entry the
// cache gets from the retried windows (SPEC §8.2.4), and logs one line.
//
// A recovered window is spliced in under both policies. A window whose route
// failed again is dropped from the served transcript under fail (a failed
// window, §8.6.13) and when the producer changed and the new default failed
// too; otherwise it keeps its cached fallback text. The cache keeps a failed
// window's fallback text unless a changed producer made it invalid: under fail
// the entry is not purged, so a return to default costs no decode until due.
func (s *Service) settleWindowRetry(doc model.Document, hit cachedTranscript, outcomes []windowRetryResult, v fallbackVerdict) fallbackRetry {
	now := s.nowUTC()
	servedDrop := !s.routeErrorFallback || v.producerChanged
	cacheDrop := s.routeErrorFallback && v.producerChanged
	served := s.spliceWindowRetry(hit, outcomes, servedDrop, now, v)
	cache := served
	if cacheDrop != servedDrop {
		cache = s.spliceWindowRetry(hit, outcomes, cacheDrop, now, v)
		// Under fail the retry left no fallback record, so the stamp of the
		// windows the cache keeps is the one they had.
		if coverageHasFallback(cache.coverage) {
			cache.coverage.RouteFallbackAt, cache.coverage.RouteFallbackBy = hit.coverage.RouteFallbackAt, hit.coverage.RouteFallbackBy
		}
	}
	s.logWindowRetry(doc, outcomes, servedDrop, now)
	if served.coverage == nil || served.coverage.WindowsDecoded == 0 {
		return fallbackRetry{cache: &cache, err: fmt.Errorf("%w: %w: transcribe %s: every decoded window was a fallback window and its retry failed (SPEC §8.2.4)", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath)}
	}
	return fallbackRetry{served: served, cache: &cache}
}

// logWindowRetry is the one §8.2.4 retry line per item: the item, the
// candidates, the number of windows retried, and the result.
func (s *Service) logWindowRetry(doc model.Document, outcomes []windowRetryResult, dropped bool, now time.Time) {
	var recovered, redecoded, failed int
	var firstErr error
	cands := map[string]bool{}
	for _, o := range outcomes {
		switch {
		case o.results != nil && o.fallbackFrom == "":
			recovered++
			cands[o.route.route] = true
		case o.results != nil:
			redecoded++
			cands[o.fallbackFrom] = true
		default:
			failed++
			cands[o.win.entry.FallbackFrom] = true
			if firstErr == nil {
				firstErr = o.err
			}
		}
	}
	names := make([]string, 0, len(cands))
	for c := range cands {
		names = append(names, c)
	}
	sort.Strings(names)
	result := "the route recovered every window"
	if redecoded > 0 {
		result = fmt.Sprintf("%d recovered, %d failed again and the current default profile decoded them once; next due %s", recovered, redecoded, s.nextDueLabel(now))
	}
	if failed > 0 {
		next := "next due " + s.nextDueLabel(now)
		if dropped {
			next = "the failed windows leave the transcript"
		}
		result = fmt.Sprintf("%d recovered, %d failed again (TRANSCRIBE_FAILED: %v); %s", recovered, failed, firstErr, next)
	}
	s.getLogger().Printf("route fallback retry %s: candidate %s, %d window(s) retried: %s (SPEC §8.2.4)",
		doc.RelPath, strings.Join(names, ","), len(outcomes), result)
}

// spliceWindowRetry applies the retried windows to a copy of the cached
// transcript: a decoded window's segments replace the cached ones in its core
// and its coverage.languages entries name the route that decoded it; a failed
// window is dropped (text, range and entries) when drop is set and kept
// otherwise. The route-fallback stamp is set when a fallback window remains
// and removed when none does.
func (s *Service) spliceWindowRetry(hit cachedTranscript, outcomes []windowRetryResult, drop bool, now time.Time, v fallbackVerdict) cachedTranscript {
	segs := windowSegments(model.TranscriptResult{Text: hit.text, Words: hit.words}, 0, 0)
	cov := copyCoverage(hit.coverage)
	for i, o := range outcomes {
		switch {
		case o.results != nil:
			segs = append(dropCoreSegments(segs, o.win), retrySegments(o, i+1)...)
			cov.Languages = relabelCore(cov.Languages, o.win.core, o.route, o.fallbackFrom)
		case drop:
			segs = dropCoreSegments(segs, o.win)
			dropCoreCoverage(cov, o.win.core)
		}
	}
	cov.Languages = coalesceCoverageLanguages(cov.Languages, cov.DurationMS)
	cov.RouteFallbackAt, cov.RouteFallbackBy = "", ""
	if coverageHasFallback(cov) {
		cov.RouteFallbackAt, cov.RouteFallbackBy = now.Format(time.RFC3339), hit.coverage.RouteFallbackBy
		if v.producerChanged {
			cov.RouteFallbackBy = s.defaultRouteIdentity()
		}
	}
	text, words := renderSegments(segs)
	return cachedTranscript{text: text, words: words, coverage: cov, route: nil}
}

// inWindowCore reports whether a segment starting at startMS belongs to the
// window's core in the merged transcript. The last window keeps everything
// from its start, as MergeTranscriptWindows does.
func inWindowCore(startMS int, w fallbackWindow) bool {
	return startMS >= w.core.StartMS && (w.last || startMS < w.core.EndMS)
}

// dropCoreSegments removes the cached segments inside a window's core.
func dropCoreSegments(segs []mergedSegment, w fallbackWindow) []mergedSegment {
	kept := segs[:0:0]
	for _, sg := range segs {
		if !inWindowCore(sg.startMS, w) {
			kept = append(kept, sg)
		}
	}
	return kept
}

// retrySegments turns a retried window's decoded pieces into absolute-time
// segments inside the window's core, de-duplicated and grouped among
// themselves exactly as the merge treats one window.
func retrySegments(o windowRetryResult, win int) []mergedSegment {
	var segs []mergedSegment
	for k, r := range o.results {
		// Each piece is its own decode, as in MergeTranscriptWindows, so the
		// de-duplication compares the pieces of a split window.
		for _, sg := range windowSegments(r.res, r.piece.startMS, win*1000+k) {
			if sg.startMS < o.win.core.StartMS {
				sg.startMS = o.win.core.StartMS
			}
			if inWindowCore(sg.startMS, o.win) {
				segs = append(segs, sg)
			}
		}
	}
	sort.SliceStable(segs, func(a, b int) bool { return segs[a].startMS < segs[b].startMS })
	return groupMistimedSegments(dedupMergedSegments(segs))
}

// renderSegments formats segments back into the `[mm:ss] text` transcript and
// its flat, time-ordered word list.
func renderSegments(segs []mergedSegment) (string, []model.TimedWord) {
	sort.SliceStable(segs, func(a, b int) bool { return segs[a].startMS < segs[b].startMS })
	var lines []string
	var words []model.TimedWord
	for _, sg := range segs {
		if strings.TrimSpace(sg.body) == "" {
			continue
		}
		lines = append(lines, formatTimestampMarker(sg.startMS)+" "+sg.body)
		words = append(words, sg.words...)
	}
	sort.SliceStable(words, func(a, b int) bool { return words[a].StartMS < words[b].StartMS })
	return strings.Join(lines, "\n"), words
}

// copyCoverage is a deep copy of a coverage record, so a splice never changes
// the record of the cache hit it started from.
func copyCoverage(c *TranscriptCoverage) *TranscriptCoverage {
	if c == nil {
		return &TranscriptCoverage{}
	}
	out := *c
	out.Ranges = append([]CoverageRange(nil), c.Ranges...)
	out.Languages = append([]model.CoverageLanguage(nil), c.Languages...)
	out.Refused = append([]model.RefusedRange(nil), c.Refused...)
	return &out
}

// relabelCore rewrites the fallback entries inside a core for a window that a
// route decoded: the part inside the core names the route, and fallback_from
// is the candidate a new default profile replaced (empty when the route itself
// decoded). The parts outside the core keep their record.
func relabelCore(entries []model.CoverageLanguage, core CoverageRange, route routedSTT, fallbackFrom string) []model.CoverageLanguage {
	var out []model.CoverageLanguage
	for _, e := range entries {
		if e.FallbackFrom == "" || e.EndMS <= core.StartMS || e.StartMS >= core.EndMS {
			out = append(out, e)
			continue
		}
		out = append(out, outsideCore(e, core)...)
		in := e
		in.StartMS, in.EndMS = max(e.StartMS, core.StartMS), min(e.EndMS, core.EndMS)
		in.Route, in.FallbackFrom, in.Covered = route.route, fallbackFrom, routeCovers(route, e.Language)
		out = append(out, in)
	}
	return out
}

// outsideCore returns the parts of an entry before and after a core.
func outsideCore(e model.CoverageLanguage, core CoverageRange) []model.CoverageLanguage {
	var out []model.CoverageLanguage
	if e.StartMS < core.StartMS {
		before := e
		before.EndMS = core.StartMS
		out = append(out, before)
	}
	if e.EndMS > core.EndMS {
		after := e
		after.StartMS = core.EndMS
		out = append(out, after)
	}
	return out
}

// dropCoreCoverage records a window whose retry failed as a failed window
// (§8.6.13): its core leaves coverage.ranges and coverage.languages, and it no
// longer counts as decoded.
func dropCoreCoverage(cov *TranscriptCoverage, core CoverageRange) {
	var entries []model.CoverageLanguage
	for _, e := range cov.Languages {
		if e.EndMS <= core.StartMS || e.StartMS >= core.EndMS {
			entries = append(entries, e)
			continue
		}
		entries = append(entries, outsideCore(e, core)...)
	}
	cov.Languages = entries
	var ranges []CoverageRange
	for _, r := range cov.Ranges {
		if r.EndMS <= core.StartMS || r.StartMS >= core.EndMS {
			ranges = append(ranges, r)
			continue
		}
		if r.StartMS < core.StartMS {
			ranges = append(ranges, CoverageRange{StartMS: r.StartMS, EndMS: core.StartMS})
		}
		if r.EndMS > core.EndMS {
			ranges = append(ranges, CoverageRange{StartMS: core.EndMS, EndMS: r.EndMS})
		}
	}
	cov.Ranges = coalesceCoverageRanges(ranges)
	cov.DecodedMS = 0
	for _, r := range cov.Ranges {
		cov.DecodedMS += r.EndMS - r.StartMS
	}
	if cov.WindowsDecoded > 0 {
		cov.WindowsDecoded--
	}
}
