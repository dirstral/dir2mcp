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
	// missing is set for a window a changed producer left with no text (a
	// route_fallback_dropped entry in the cache): a recovered retry adds its
	// text, range and entry instead of replacing them.
	missing bool
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
// transcript (SPEC §8.2.4). Every window is cut before any is decoded. A host
// that cannot extract, stage, probe or cut the audio, or a rebuilt schedule
// that does not match the cached record, means the retry cannot run; that is
// settled by windowRetryNotRun, not as a route failure.
func (s *Service) retryWindowFallback(ctx context.Context, doc model.Document, content []byte, hit cachedTranscript, v RouteFallbackDecision) fallbackRetry {
	relPath, audio, ok, err := s.transcriptInput(ctx, doc, content)
	if err != nil {
		return s.windowRetryNotRun(doc, hit, err)
	}
	if !ok {
		return s.windowRetryNotRun(doc, hit, fmt.Errorf("no audio track to decode"))
	}
	tmpPath, cleanup, err := stageMediaTemp(audio, filepath.Ext(relPath))
	if err != nil {
		return s.windowRetryNotRun(doc, hit, err)
	}
	defer cleanup()
	rp := s.rebuildWindowPlan(ctx, tmpPath, audio, relPath, hit.coverage)
	wins := fallbackWindows(rp, hit.coverage)
	if err := scheduleMatches(rp, hit.coverage, wins); err != nil {
		return s.windowRetryNotRun(doc, hit, err)
	}
	cuts := make([][]windowPiece, len(wins))
	for i, w := range wins {
		pieces, err := s.retryPieces(ctx, rp, w)
		if err != nil {
			return s.windowRetryNotRun(doc, hit, fmt.Errorf("cut window [%d,%d)ms: %w", w.startMS, w.endMS, err))
		}
		cuts[i] = pieces
	}
	var outcomes []windowRetryResult
	for i, w := range wins {
		outcomes = append(outcomes, s.retryOneWindow(ctx, rp, w, cuts[i], v))
	}
	return s.settleWindowRetry(doc, hit, outcomes, v)
}

// windowRetryNotRun settles a window retry the host could not run (SPEC
// §8.2.4). Under default the cached fallback text is served, the document
// stays indexed, route_fallback_at moves to now so the retry waits one
// interval, and route_fallback_skip_reason records why. Under fail the spec
// does not allow serving fallback text, so the fallback windows leave the
// served transcript as failed windows (the cache keeps their text and the
// transcript records them as dropped), exactly as when the route fails.
func (s *Service) windowRetryNotRun(doc model.Document, hit cachedTranscript, cause error) fallbackRetry {
	now := s.nowUTC()
	reason := cause.Error()
	if s.routeErrorFallback {
		out := hit
		out.coverage = copyCoverage(hit.coverage)
		out.coverage.RouteFallbackAt = now.Format(time.RFC3339)
		out.coverage.RouteFallbackSkipReason = reason
		s.getLogger().Printf("route fallback retry %s: could not run (%v); the cached fallback text stays indexed; next due %s (SPEC §8.2.4)", doc.RelPath, cause, s.nextDueLabel(now))
		return fallbackRetry{served: out, cache: &out}
	}
	served := dropAllFallback(hit, true)
	served.coverage.RouteFallbackSkipReason = reason
	s.getLogger().Printf("route fallback retry %s: could not run (%v); media.stt.on_route_error=fail, so the fallback windows leave the transcript (SPEC §8.2.4)", doc.RelPath, cause)
	if served.coverage.WindowsDecoded == 0 {
		return fallbackRetry{err: fmt.Errorf("%w: %w: transcribe %s: the route-fallback retry could not run (%v) and every decoded window was a fallback window", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath, cause)}
	}
	return fallbackRetry{served: served}
}

// dropAllFallback is a cached window-scoped transcript without its fallback
// windows, cut on the cores of the cached decode schedule (the same cores the
// served splice uses). markDropped records them as dropped windows whose text
// the cache keeps. A cache entry that predates the recorded schedule has no
// window size; its cores are then the entry ranges, which the coalesced
// entries span exactly when no window failed in between.
func dropAllFallback(hit cachedTranscript, markDropped bool) cachedTranscript {
	segs := windowSegments(model.TranscriptResult{Text: hit.text, Words: hit.words}, 0, 0)
	cov := copyCoverage(hit.coverage)
	var dropped []model.CoverageLanguage
	for _, w := range cachedFallbackWindows(hit.coverage) {
		if w.missing {
			continue
		}
		dropped = append(dropped, fallbackEntriesInCore(cov.Languages, w.core)...)
		segs = dropCoreSegments(segs, w)
		dropCoreCoverage(cov, w.core)
	}
	cov.RouteFallbackAt, cov.RouteFallbackBy = "", ""
	cov.RouteFallbackDropped, cov.RouteFallbackDroppedCached = nil, false
	if markDropped && len(dropped) > 0 {
		cov.RouteFallbackAt, cov.RouteFallbackBy = hit.coverage.RouteFallbackAt, hit.coverage.RouteFallbackBy
		cov.RouteFallbackDropped, cov.RouteFallbackDroppedCached = append(dropped, hit.coverage.RouteFallbackDropped...), true
	}
	text, words := renderSegments(segs)
	return cachedTranscript{text: text, words: words, coverage: cov}
}

// cachedFallbackWindows is fallbackWindows on the schedule the cache records.
func cachedFallbackWindows(coverage *TranscriptCoverage) []fallbackWindow {
	if rp, ok := cachedWindowPlan(coverage); ok {
		return fallbackWindows(rp, coverage)
	}
	var out []fallbackWindow
	for _, e := range coverageLanguages(coverage) {
		if e.FallbackFrom != "" {
			last := coverage.DurationMS > 0 && e.EndMS >= coverage.DurationMS
			out = append(out, fallbackWindow{startMS: e.StartMS, endMS: e.EndMS, core: CoverageRange{StartMS: e.StartMS, EndMS: e.EndMS}, last: last, entry: e})
		}
	}
	return out
}

// routeFallbackDurationToleranceMS is how far the probed recording length may
// drift from the cached one (an ffprobe rounding change) before the cached
// schedule is no longer trusted.
const routeFallbackDurationToleranceMS = 1000

// cachedWindowPlan is the decode schedule the cache records: its recording
// length and decode window. ok is false for a cache entry that predates the
// recorded window.
func cachedWindowPlan(coverage *TranscriptCoverage) (windowRetryPlan, bool) {
	if coverage == nil || coverage.DurationMS <= 0 || coverage.WindowMS <= 0 {
		return windowRetryPlan{}, false
	}
	return windowPlanFor(coverage.DurationMS, coverage.WindowMS, 0), true
}

// windowPlanFor is the schedule decodeWindowedTranscript builds for a length
// and a window; a window as long as the recording is one window.
func windowPlanFor(totalMS, windowMS, capBytes int) windowRetryPlan {
	if windowMS <= 0 || windowMS >= totalMS {
		return windowRetryPlan{plan: windowSchedule{totalMS: totalMS, windowMS: totalMS, stepMS: totalMS, capBytes: capBytes, label: "transcription"}, starts: []int{0}}
	}
	overlapMS := TranscriptWindowOverlapMS(windowMS)
	stepMS := windowMS - overlapMS
	if stepMS <= 0 {
		stepMS = windowMS
	}
	return windowRetryPlan{
		plan:   windowSchedule{totalMS: totalMS, windowMS: windowMS, stepMS: stepMS, capBytes: capBytes, label: "transcription"},
		starts: TranscriptWindowStarts(totalMS, stepMS, overlapMS),
	}
}

// scheduleMatches checks that the rebuilt schedule is the one the cached
// record was decoded under: a known length within the tolerance of the cached
// one, the same window count, and every fallback or dropped entry inside a
// window the retry decodes. A mismatch would splice text at the wrong place,
// so the retry does not run.
func scheduleMatches(rp windowRetryPlan, coverage *TranscriptCoverage, wins []fallbackWindow) error {
	if rp.plan.totalMS <= 0 {
		return fmt.Errorf("the recording length is unknown")
	}
	if coverage == nil {
		return nil
	}
	if coverage.DurationMS > 0 && absInt(coverage.DurationMS-rp.plan.totalMS) > routeFallbackDurationToleranceMS {
		return fmt.Errorf("the recording is %d ms now and %d ms in the cached record", rp.plan.totalMS, coverage.DurationMS)
	}
	if coverage.WindowsAttempted > 0 && coverage.WindowsAttempted != len(rp.starts) {
		return fmt.Errorf("the decode schedule has %d windows now and %d in the cached record", len(rp.starts), coverage.WindowsAttempted)
	}
	for _, e := range append(append([]model.CoverageLanguage(nil), coverageLanguages(coverage)...), coverage.RouteFallbackDropped...) {
		if e.FallbackFrom != "" && !entryInWindows(e, wins) {
			return fmt.Errorf("the fallback entry [%d,%d)ms does not match the rebuilt decode schedule", e.StartMS, e.EndMS)
		}
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// entryInWindows reports whether an entry overlaps one of the windows.
func entryInWindows(e model.CoverageLanguage, wins []fallbackWindow) bool {
	for _, w := range wins {
		if e.StartMS < w.core.EndMS && e.EndMS > w.core.StartMS {
			return true
		}
	}
	return false
}

// rebuildWindowPlan rebuilds the decode schedule the windowed decode used, so
// a retried window has the same extent and core it had then (SPEC §8.6.13).
// The schedule the cache records is used when the probed length is within
// the tolerance of the cached one (or the probe fails). Otherwise the
// schedule is computed as the decode computed it, from the probed length.
func (s *Service) rebuildWindowPlan(ctx context.Context, tmpPath string, audio []byte, relPath string, coverage *TranscriptCoverage) windowRetryPlan {
	totalMS := s.probeStagedDurationMS(ctx, tmpPath)
	capBytes := sttPayloadCapBytes(s.transcriber)
	var rp windowRetryPlan
	if coverage != nil && coverage.DurationMS > 0 && (totalMS <= 0 || absInt(totalMS-coverage.DurationMS) <= routeFallbackDurationToleranceMS) {
		totalMS = coverage.DurationMS
	}
	if cached, ok := cachedWindowPlan(coverage); ok && totalMS == coverage.DurationMS {
		rp = cached
		rp.plan.capBytes = capBytes
	} else {
		windowMS := STTWindowMS(totalMS, len(audio), capBytes)
		rp = windowPlanFor(totalMS, windowMS, capBytes)
	}
	rp.tmpPath, rp.audio, rp.relPath = tmpPath, audio, relPath
	return rp
}

// fallbackWindows lists the scheduled windows whose core overlaps a
// coverage.languages entry that records fallback_from, or a dropped entry a
// changed producer left with no text, in schedule order.
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
		w := fallbackWindow{startMS: start, endMS: end, core: core, last: i == len(rp.starts)-1}
		if e, ok := fallbackEntryIn(coverageLanguages(coverage), core); ok {
			w.entry = e
			out = append(out, w)
		} else if e, ok := fallbackEntryIn(missingDropped(coverage), core); ok {
			w.entry, w.missing = e, true
			out = append(out, w)
		}
	}
	return out
}

// missingDropped is the dropped entries of a cached coverage record whose
// windows have no text in the cache.
func missingDropped(coverage *TranscriptCoverage) []model.CoverageLanguage {
	if coverage == nil || coverage.RouteFallbackDroppedCached {
		return nil
	}
	return coverage.RouteFallbackDropped
}

// fallbackEntryIn returns the first fallback entry that overlaps core.
func fallbackEntryIn(entries []model.CoverageLanguage, core CoverageRange) (model.CoverageLanguage, bool) {
	for _, e := range entries {
		if e.FallbackFrom != "" && e.StartMS < core.EndMS && e.EndMS > core.StartMS {
			return e, true
		}
	}
	return model.CoverageLanguage{}, false
}

// retryOneWindow decodes one fallback window's pieces on its route. A retry
// succeeds only when every piece decodes and the per-window floor and quality
// gate keep the text, so the window's coverage range is unchanged. When the
// route fails and the producer changed under on_route_error: default, the
// current default profile decodes the window once instead (SPEC §8.2.4).
func (s *Service) retryOneWindow(ctx context.Context, rp windowRetryPlan, w fallbackWindow, pieces []windowPiece, v RouteFallbackDecision) windowRetryResult {
	lang := w.entry.Language
	cand, err := s.retryCandidate(lang, w.entry.FallbackFrom)
	if err == nil {
		results, derr := s.decodePieces(ctx, rp.relPath, cand.stt, pieces, rp.plan)
		err = s.retryDecodeError(results, pieces, derr, cand, lang, w.core)
		if err == nil {
			return windowRetryResult{win: w, results: results, route: cand}
		}
	}
	if !v.ProducerChanged || !s.routeErrorFallback || ctx.Err() != nil {
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
// Every dropped window is recorded (route_fallback_dropped). Under fail the
// cache keeps its text, so the gate processes the transcript on every run: a
// repaired checkpoint takes over, and a return to default restores the cached
// fallback text. After a producer change no text is left, so the window is
// retried when due on its route.
func (s *Service) settleWindowRetry(doc model.Document, hit cachedTranscript, outcomes []windowRetryResult, v RouteFallbackDecision) fallbackRetry {
	now := s.nowUTC()
	underFail := !s.routeErrorFallback
	servedDrop := underFail || v.ProducerChanged
	cacheDrop := !underFail && v.ProducerChanged
	served := s.spliceWindowRetry(hit, outcomes, servedDrop, underFail, now, v)
	cache := served
	if cacheDrop != servedDrop {
		cache = s.spliceWindowRetry(hit, outcomes, cacheDrop, false, now, v)
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
// and its coverage.languages entries name the route that decoded it (a window
// with no cached text gets its text, range and entry added); a failed window
// is dropped (text, range and entries) when drop is set and kept otherwise.
// Every dropped window is recorded in route_fallback_dropped. cachedText says
// that the cache keeps the text of the windows this splice drops (a drop
// under fail). The route-fallback stamp is set while a fallback window or a
// dropped window remains and removed when none does.
func (s *Service) spliceWindowRetry(hit cachedTranscript, outcomes []windowRetryResult, drop, cachedText bool, now time.Time, v RouteFallbackDecision) cachedTranscript {
	segs := windowSegments(model.TranscriptResult{Text: hit.text, Words: hit.words}, 0, 0)
	cov := copyCoverage(hit.coverage)
	carried := append([]model.CoverageLanguage(nil), missingDropped(hit.coverage)...)
	var dropped []model.CoverageLanguage
	for i, o := range outcomes {
		switch {
		case o.results != nil && o.win.missing:
			segs = append(segs, retrySegments(o, i+1)...)
			addRecoveredWindow(cov, o)
			carried = removeCore(carried, o.win.core)
		case o.results != nil:
			segs = append(dropCoreSegments(segs, o.win), retrySegments(o, i+1)...)
			cov.Languages = relabelCore(cov.Languages, o.win.core, o.route, o.fallbackFrom)
		case drop && !o.win.missing:
			dropped = append(dropped, fallbackEntriesInCore(cov.Languages, o.win.core)...)
			segs = dropCoreSegments(segs, o.win)
			dropCoreCoverage(cov, o.win.core)
		}
	}
	cov.Languages = coalesceCoverageLanguages(cov.Languages, cov.DurationMS)
	s.stampSplice(cov, hit, carried, dropped, cachedText, now, v)
	text, words := renderSegments(segs)
	return cachedTranscript{text: text, words: words, coverage: cov, route: nil}
}

// stampSplice sets the dropped-window record and the stamp of a spliced
// coverage record. A window dropped while the cache keeps its text keeps the
// cached stamp, so the next retry follows the cached record. Otherwise a
// remaining fallback or dropped window takes the retry time and the producer
// that decoded it. The retry ran, so no skip reason remains.
func (s *Service) stampSplice(cov *TranscriptCoverage, hit cachedTranscript, carried, dropped []model.CoverageLanguage, cachedText bool, now time.Time, v RouteFallbackDecision) {
	cov.RouteFallbackAt, cov.RouteFallbackBy, cov.RouteFallbackSkipReason = "", "", ""
	cov.RouteFallbackDropped = append(append([]model.CoverageLanguage(nil), dropped...), carried...)
	cov.RouteFallbackDroppedCached = cachedText && len(dropped) > 0
	if len(cov.RouteFallbackDropped) == 0 {
		cov.RouteFallbackDropped = nil
	}
	switch {
	case cov.RouteFallbackDroppedCached && !coverageHasFallback(cov) && len(carried) == 0:
		cov.RouteFallbackAt, cov.RouteFallbackBy = hit.coverage.RouteFallbackAt, hit.coverage.RouteFallbackBy
	case coverageHasFallback(cov) || len(cov.RouteFallbackDropped) > 0:
		cov.RouteFallbackAt, cov.RouteFallbackBy = now.Format(time.RFC3339), hit.coverage.RouteFallbackBy
		if v.ProducerChanged {
			cov.RouteFallbackBy = s.defaultRouteIdentity()
		}
	}
}

// addRecoveredWindow records a recovered window that had no cached text: its
// decoded range, a coverage.languages entry for its core, and one more decoded
// window.
func addRecoveredWindow(cov *TranscriptCoverage, o windowRetryResult) {
	entry := o.win.entry
	entry.StartMS, entry.EndMS = max(entry.StartMS, o.win.core.StartMS), min(entry.EndMS, o.win.core.EndMS)
	entry.Route, entry.FallbackFrom, entry.Covered = o.route.route, o.fallbackFrom, routeCovers(o.route, entry.Language)
	cov.Languages = append(cov.Languages, entry)
	for _, r := range o.results {
		cov.Ranges = append(cov.Ranges, CoverageRange{StartMS: r.piece.startMS, EndMS: r.piece.endMS})
	}
	cov.Ranges = coalesceCoverageRanges(cov.Ranges)
	cov.DecodedMS = 0
	for _, r := range cov.Ranges {
		cov.DecodedMS += r.EndMS - r.StartMS
	}
	cov.WindowsDecoded++
}

// removeCore removes the parts of entries inside a core.
func removeCore(entries []model.CoverageLanguage, core CoverageRange) []model.CoverageLanguage {
	var out []model.CoverageLanguage
	for _, e := range entries {
		if e.EndMS <= core.StartMS || e.StartMS >= core.EndMS {
			out = append(out, e)
			continue
		}
		out = append(out, outsideCore(e, core)...)
	}
	return out
}

// fallbackEntriesInCore returns the parts of the fallback entries inside a
// core, for the route_fallback_dropped record.
func fallbackEntriesInCore(entries []model.CoverageLanguage, core CoverageRange) []model.CoverageLanguage {
	var out []model.CoverageLanguage
	for _, e := range entries {
		if e.FallbackFrom == "" || e.EndMS <= core.StartMS || e.StartMS >= core.EndMS {
			continue
		}
		e.StartMS, e.EndMS = max(e.StartMS, core.StartMS), min(e.EndMS, core.EndMS)
		out = append(out, e)
	}
	return out
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
	out.RouteFallbackDropped = append([]model.CoverageLanguage(nil), c.RouteFallbackDropped...)
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
