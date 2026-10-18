package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// This file implements the SPEC §8.2.4 rule that a route-fallback result is
// provisional (spec 0.82.0, dir2mcp #1122).
//
// Under media.stt.on_route_error: default a route candidate that fails is
// replaced once by the default STT profile, and the transcript records
// route_fallback_from (item scope) or coverage.languages[].fallback_from
// (window scope). That text is cached under the same derivation identity as a
// route decode, because the policy is not part of the identity. Without the
// rules below the cached fallback text was served for ever: after the operator
// chose on_route_error: fail, and after the failed checkpoint worked again.
//
// The rules, before a cached transcript is used:
//   - under fail, a fallback result is stale: its route decodes it again, and
//     a failure fails the item or the window and retires the fallback text;
//   - under default, a fallback result is due once route_fallback_at is older
//     than media.stt.route_retry_after (or unknown); a due result is decoded
//     once on its route, and a failure keeps the cached text and resets the
//     time, with no second decode on the default profile;
//   - under both, a fallback result whose route_fallback_by does not name the
//     current default profile is stale at once.

// errRouteFallbackRetired marks an item whose fallback text must leave the
// index: its route failed again and the fallback text cannot be served
// (SPEC §8.2.4). The caller retires the track's transcripts as §8.6.13 retires
// a refused transcript, then reports the failure (TRANSCRIBE_FAILED).
var errRouteFallbackRetired = errors.New("route-fallback transcript retired")

// routeFallbackClockSkew is how far in the future a route_fallback_at may be
// before it is not trusted. A later time (a clock that moved back, or a
// stamp from another host) would delay the retry without limit, so it is
// treated as due.
const routeFallbackClockSkew = 5 * time.Minute

// nowUTC is the current time in UTC from the injected clock, or time.Now.
func (s *Service) nowUTC() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// defaultRouteIdentity is the "<profile>|<model>" of the current default STT
// profile, the form route_fallback_by records (SPEC §8.2.4).
func (s *Service) defaultRouteIdentity() string {
	return strings.TrimSpace(s.sttProvider) + "|" + strings.TrimSpace(s.sttModel)
}

// coverageHasFallback reports whether a window-scoped coverage record holds a
// fallback window: an entry whose fallback_from names a failed candidate.
func coverageHasFallback(coverage *TranscriptCoverage) bool {
	for _, e := range coverageLanguages(coverage) {
		if e.FallbackFrom != "" {
			return true
		}
	}
	return false
}

// stampFreshRouteFallback records the §8.2.4 stamp on a transcript this run
// just decoded: the time now and the current default profile, when the decode
// left a fallback record; nothing otherwise.
func (s *Service) stampFreshRouteFallback(coverage *TranscriptCoverage, route *itemRoute) {
	at, by := s.nowUTC().Format(time.RFC3339), s.defaultRouteIdentity()
	if route != nil && route.FallbackFrom != "" {
		route.FallbackAt, route.FallbackBy = at, by
	}
	if coverageHasFallback(coverage) {
		coverage.RouteFallbackAt, coverage.RouteFallbackBy = at, by
	}
}

// fallbackStamp returns the route_fallback_at and route_fallback_by of a
// cached transcript, and whether it is a fallback result at all.
func (c cachedTranscript) fallbackStamp() (at, by string, ok bool) {
	if c.route != nil && c.route.FallbackFrom != "" {
		return c.route.FallbackAt, c.route.FallbackBy, true
	}
	if coverageHasFallback(c.coverage) || len(missingDropped(c.coverage)) > 0 {
		return c.coverage.RouteFallbackAt, c.coverage.RouteFallbackBy, true
	}
	return "", "", false
}

// RouteFallbackPolicy is the configuration a §8.2.4 due decision reads: the
// on_route_error policy, media.stt.route_retry_after, and the
// "<profile>|<model>" of the current default STT profile.
type RouteFallbackPolicy struct {
	Fail       bool
	RetryAfter time.Duration
	Producer   string
}

// RouteFallbackDecision is the §8.2.4 decision on one fallback result: whether
// it is decoded again on this run, whether the default profile that produced
// it changed (the result is then stale under both policies), why, and when a
// result that is not due becomes due.
type RouteFallbackDecision struct {
	Due             bool
	ProducerChanged bool
	Why             string
	NextDue         time.Time
}

// RouteFallbackDropped is the dropped-window state of a fallback result: none,
// windows whose fallback text the cache keeps (dropped under fail), or windows
// with no text left (dropped after a producer change).
type RouteFallbackDropped int

const (
	DroppedNone RouteFallbackDropped = iota
	DroppedCached
	DroppedWithoutText
)

// droppedState names the dropped-window state of a record.
func droppedState(dropped, cached bool) RouteFallbackDropped {
	switch {
	case !dropped:
		return DroppedNone
	case cached:
		return DroppedCached
	default:
		return DroppedWithoutText
	}
}

// DecideRouteFallback applies the §8.2.4 rules to one fallback result. It is
// the one due computation: the re-derivation gate, the cache read and
// `dir2mcp doctor` all call it. A transcript with windows dropped while the
// cache keeps their text (DroppedCached) is processed on every run, so its
// route takes over once it works and a return to default restores the cached
// fallback text. Windows dropped with no text left follow the time rules.
func DecideRouteFallback(p RouteFallbackPolicy, at, by string, dropped RouteFallbackDropped, now time.Time) RouteFallbackDecision {
	by = strings.TrimSpace(by)
	switch {
	case by == "" || by != strings.TrimSpace(p.Producer):
		return RouteFallbackDecision{Due: true, ProducerChanged: true, Why: fmt.Sprintf("route_fallback_by %q is not the current default profile %q", by, p.Producer)}
	case dropped == DroppedCached:
		return RouteFallbackDecision{Due: true, Why: "a fallback window was dropped under media.stt.on_route_error=fail"}
	case p.Fail:
		return RouteFallbackDecision{Due: true, Why: "media.stt.on_route_error=fail"}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	switch {
	case err != nil:
		return RouteFallbackDecision{Due: true, Why: "no valid route_fallback_at"}
	case t.After(now.Add(routeFallbackClockSkew)):
		return RouteFallbackDecision{Due: true, Why: "route_fallback_at is in the future"}
	case p.RetryAfter <= 0:
		return RouteFallbackDecision{Due: true, Why: "media.stt.route_retry_after=0"}
	}
	next := t.Add(p.RetryAfter)
	if now.Before(next) {
		return RouteFallbackDecision{NextDue: next}
	}
	return RouteFallbackDecision{Due: true, Why: "route_retry_after elapsed"}
}

// routeFallbackPolicy is the Service's current §8.2.4 configuration.
func (s *Service) routeFallbackPolicy() RouteFallbackPolicy {
	return RouteFallbackPolicy{Fail: !s.routeErrorFallback, RetryAfter: s.routeRetryAfter, Producer: s.defaultRouteIdentity()}
}

// routeFallbackVerdict decides a cached fallback result's stamp.
func (s *Service) routeFallbackVerdict(at, by string) RouteFallbackDecision {
	return DecideRouteFallback(s.routeFallbackPolicy(), at, by, DroppedNone, s.nowUTC())
}

// routeFallbackMeta is the part of a transcript meta_json the §8.2.4 gate reads.
type routeFallbackMeta struct {
	RouteFallbackFrom          string                   `json:"route_fallback_from"`
	RouteFallbackAt            string                   `json:"route_fallback_at"`
	RouteFallbackBy            string                   `json:"route_fallback_by"`
	RouteFallbackDropped       []model.CoverageLanguage `json:"route_fallback_dropped"`
	RouteFallbackDroppedCached bool                     `json:"route_fallback_dropped_cached"`
	Coverage                   *TranscriptCoverage      `json:"coverage"`
}

// record is the meta as a fallback record, and whether it holds one.
func (m routeFallbackMeta) record(relPath, repType string) (model.RouteFallbackRecord, bool) {
	dropped := len(m.RouteFallbackDropped) > 0
	if m.RouteFallbackFrom == "" && !coverageHasFallback(m.Coverage) && !dropped {
		return model.RouteFallbackRecord{}, false
	}
	return model.RouteFallbackRecord{RelPath: relPath, RepType: repType, At: m.RouteFallbackAt, By: m.RouteFallbackBy,
		Dropped: dropped, DroppedCached: dropped && m.RouteFallbackDroppedCached}, true
}

// RouteFallbackDue reports whether a stored transcript meta_json holds a
// fallback result that this run must decode again (SPEC §8.2.4), and why. A
// meta_json with no fallback record, or one that does not parse, is never due.
func (s *Service) RouteFallbackDue(metaJSON string) (bool, string) {
	var meta routeFallbackMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return false, ""
	}
	r, ok := meta.record("", "")
	if !ok {
		return false, ""
	}
	d := DecideRouteFallback(s.routeFallbackPolicy(), r.At, r.By, droppedState(r.Dropped, r.DroppedCached), s.nowUTC())
	return d.Due, d.Why
}

// routeFallbackLister is the optional store capability behind the §8.2.4 gate:
// the live transcripts that hold a fallback record, for the whole corpus (one
// query per scan) or for one document.
type routeFallbackLister interface {
	RouteFallbackTranscripts(ctx context.Context) ([]model.RouteFallbackRecord, error)
	RouteFallbackTranscriptsFor(ctx context.Context, relPath string) ([]model.RouteFallbackRecord, error)
}

// loadRouteFallbackIndex reads every fallback record once at the start of a
// scan, so the gate costs one query per scan instead of one per unchanged
// document. A store without the capability, or a failed query, leaves the
// gate on its per-document path.
func (s *Service) loadRouteFallbackIndex(ctx context.Context) {
	lister, ok := s.store.(routeFallbackLister)
	if !ok || s.transcriber == nil {
		return
	}
	records, err := lister.RouteFallbackTranscripts(ctx)
	if err != nil {
		s.getLogger().Printf("route fallback: cannot list route-fallback transcripts (%v); checking each media document instead", err)
		return
	}
	index := map[string][]model.RouteFallbackRecord{}
	for _, r := range records {
		index[r.RelPath] = append(index[r.RelPath], r)
	}
	s.routeFallbackMu.Lock()
	s.routeFallbackIndex = index
	s.routeFallbackMu.Unlock()
}

// clearRouteFallbackIndex drops the per-scan index when the scan ends.
func (s *Service) clearRouteFallbackIndex() {
	s.routeFallbackMu.Lock()
	s.routeFallbackIndex = nil
	s.routeFallbackMu.Unlock()
}

// routeFallbackRecordsFor returns the fallback records of one document. During
// a scan the per-scan index answers, whatever the document type. Outside a scan
// only audio and video hold a decoded transcript, so other documents cost no
// query; docType is the type the store records (a sniffed audio file under an
// unusual extension is audio there), and the path's extension classifies a
// document the store has no type for. Every source transcript track is
// covered: an additional audio track (§8.6.12) is its own decode with its own
// fallback record.
func (s *Service) routeFallbackRecordsFor(ctx context.Context, reader representationMetaReader, relPath, docType string) []model.RouteFallbackRecord {
	s.routeFallbackMu.Lock()
	index := s.routeFallbackIndex
	s.routeFallbackMu.Unlock()
	if index != nil {
		return index[relPath]
	}
	if strings.TrimSpace(docType) == "" {
		docType = ClassifyDocType(relPath)
	}
	if docType != "audio" && docType != "video" {
		return nil
	}
	if lister, ok := s.store.(routeFallbackLister); ok {
		records, err := lister.RouteFallbackTranscriptsFor(ctx, relPath)
		if err != nil {
			return nil
		}
		return records
	}
	meta, err := reader.RepresentationMetaByType(ctx, relPath, RepTypeTranscript)
	if err != nil || strings.TrimSpace(meta) == "" {
		return nil
	}
	var m routeFallbackMeta
	if json.Unmarshal([]byte(meta), &m) != nil {
		return nil
	}
	if r, ok := m.record(relPath, RepTypeTranscript); ok {
		return []model.RouteFallbackRecord{r}
	}
	return nil
}

// routeFallbackStale is the §8.2.4 half of the §8.6.7 re-derivation gate: an
// unchanged document whose stored transcript holds a due fallback result is
// processed again, so its route decodes it. A result that is not due costs
// nothing.
func (s *Service) routeFallbackStale(ctx context.Context, reader representationMetaReader, relPath, docType string) bool {
	if s.transcriber == nil {
		return false
	}
	policy, now := s.routeFallbackPolicy(), s.nowUTC()
	for _, r := range s.routeFallbackRecordsFor(ctx, reader, relPath, docType) {
		if d := DecideRouteFallback(policy, r.At, r.By, droppedState(r.Dropped, r.DroppedCached), now); d.Due {
			s.getLogger().Printf("route fallback: %s (%s) holds a route-fallback transcript (%s); decoding it again on its route this scan (SPEC §8.2.4)", relPath, r.RepType, d.Why)
			return true
		}
	}
	return false
}

// fallbackRetry is the outcome of one §8.2.4 retry: the transcript the index
// gets, the entry the cache gets (nil leaves the cache entry as it is), and the
// item's failure.
type fallbackRetry struct {
	served cachedTranscript
	cache  *cachedTranscript
	err    error
}

// serveCachedTranscript returns a cached transcript, after the §8.2.4 retry
// when it is a fallback result that is due. retry is false on the derivation
// pass (§8.6.11), which reads the text the transcription pass settled and
// never decodes a fallback result a second time in one run.
func (s *Service) serveCachedTranscript(ctx context.Context, doc model.Document, content []byte, paths transcriptCachePaths, hit cachedTranscript, retry bool) (string, []model.TimedWord, *TranscriptCoverage, *itemRoute, error) {
	at, by, ok := hit.fallbackStamp()
	if !ok {
		return hit.unpack()
	}
	if !retry {
		return withoutFallbackUnderFail(hit, s.routeErrorFallback).unpack()
	}
	v := s.routeFallbackVerdict(at, by)
	if !v.Due {
		return hit.unpack()
	}
	if err := ctx.Err(); err != nil {
		return "", nil, nil, nil, retryInterrupted(doc, err)
	}
	var r fallbackRetry
	if hit.route != nil && hit.route.FallbackFrom != "" {
		r = s.retryItemFallback(ctx, doc, content, hit, v)
	} else {
		r = s.retryWindowFallback(ctx, doc, content, hit, v)
	}
	// A cancelled run decides nothing: the cache, the stamp and the index stay.
	if err := ctx.Err(); err != nil {
		return "", nil, nil, nil, retryInterrupted(doc, err)
	}
	if r.cache != nil {
		s.publishRetriedCache(doc, paths, hit, *r.cache)
	}
	if r.err != nil {
		return "", nil, nil, nil, r.err
	}
	return r.served.unpack()
}

// withoutFallbackUnderFail is a cached fallback result as the derivation pass
// may read it. Under default it is the hit. Under fail the cache keeps the
// fallback text that the transcription pass took out of the index, so the
// derivation pass must not translate it: an item reads as no transcript, and
// a window-scoped transcript loses its fallback windows on the same cores the
// served splice uses (dropAllFallback).
func withoutFallbackUnderFail(hit cachedTranscript, fallsBack bool) cachedTranscript {
	if fallsBack {
		return hit
	}
	if hit.route != nil && hit.route.FallbackFrom != "" {
		return cachedTranscript{}
	}
	return dropAllFallback(hit, false)
}

// retryInterrupted is the error of a retry the run's context cancelled.
func retryInterrupted(doc model.Document, err error) error {
	return fmt.Errorf("%w: transcribe %s: route-fallback retry interrupted: %w", ErrTranscriptProviderFailure, doc.RelPath, err)
}

// publishRetriedCache writes a retry's cache entry. When the text did not
// change (a failed retry that only moved the stamp) only the sidecars are
// written, so the text and word timing are not rewritten for a time stamp.
func (s *Service) publishRetriedCache(doc model.Document, paths transcriptCachePaths, hit, entry cachedTranscript) {
	if entry.text == hit.text {
		if s.publishCachedCoverage(paths.coverage, entry.coverage) {
			s.publishCachedItemRoute(paths.route, entry.route)
		}
		return
	}
	if err := s.publishTranscriptCache(paths, entry); err != nil {
		s.getLogger().Printf("route fallback retry %s: %v; the next run decodes it again", doc.RelPath, err)
	}
}

// retryItemFallback is the §8.2.4 retry under item scope: the item is decoded
// once on the route for its recorded language. The identifier is not asked
// again: its binding and probe are in the identity, so they are unchanged.
func (s *Service) retryItemFallback(ctx context.Context, doc model.Document, content []byte, hit cachedTranscript, v RouteFallbackDecision) fallbackRetry {
	rec := *hit.route
	relPath, audio, ok, err := s.transcriptInput(ctx, doc, content)
	if err != nil {
		return s.itemRetryNotRun(doc, hit, err)
	}
	if !ok {
		return s.itemRetryNotRun(doc, hit, errors.New("no audio track to decode"))
	}
	cand := rec.FallbackFrom
	route, err := s.retryCandidate(rec.Language, rec.FallbackFrom)
	if err == nil {
		cand = route.route
		var text string
		var words []model.TimedWord
		var cov *TranscriptCoverage
		text, words, cov, err = s.transcribeStructuredWindowed(ctx, relPath, audio, route.stt)
		if err == nil {
			out := cachedTranscript{text: normalizeTranscriptNewlines(text), words: words, coverage: cov,
				route: &itemRoute{Language: rec.Language, Confidence: rec.Confidence, Route: route.route, Uncovered: !routeCovers(route, rec.Language)}}
			s.getLogger().Printf("route fallback retry %s: candidate %q recovered; its text replaces the fallback text (%s, SPEC §8.2.4)", doc.RelPath, cand, v.Why)
			return fallbackRetry{served: out, cache: &out}
		}
	}
	if ctx.Err() != nil {
		return fallbackRetry{}
	}
	return s.itemRetryFailed(ctx, doc, relPath, audio, hit, cand, v, err)
}

// itemRetryNotRun settles an item retry the host could not run (SPEC §8.2.4).
// Under default the cached fallback text is served, the document stays
// indexed, route_fallback_at moves to now so the retry waits one interval, and
// route_fallback_skip_reason records why. Under fail the spec does not allow
// serving fallback text, so the item fails and its fallback text is retired,
// exactly as when the route fails.
func (s *Service) itemRetryNotRun(doc model.Document, hit cachedTranscript, cause error) fallbackRetry {
	if !s.routeErrorFallback {
		s.getLogger().Printf("route fallback retry %s: could not run (%v); media.stt.on_route_error=fail, so the item fails and its fallback text leaves the index (SPEC §8.2.4)", doc.RelPath, cause)
		return fallbackRetry{err: fmt.Errorf("%w: %w: transcribe %s: the route-fallback retry could not run: %w", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath, cause)}
	}
	now := s.nowUTC()
	rec := *hit.route
	rec.FallbackAt, rec.SkipReason = now.Format(time.RFC3339), cause.Error()
	out := hit
	out.route = &rec
	s.getLogger().Printf("route fallback retry %s: could not run (%v); the cached fallback text stays indexed; next due %s (SPEC §8.2.4)", doc.RelPath, cause, s.nextDueLabel(now))
	return fallbackRetry{served: out, cache: &out}
}

// itemRetryFailed settles an item whose route failed again (SPEC §8.2.4):
// under fail the item fails and its fallback text is retired; under default a
// changed producer decodes it once on the new default profile; otherwise the
// cached fallback text stays and route_fallback_at moves to now.
func (s *Service) itemRetryFailed(ctx context.Context, doc model.Document, relPath string, audio []byte, hit cachedTranscript, cand string, v RouteFallbackDecision, cause error) fallbackRetry {
	rec := *hit.route
	if !s.routeErrorFallback {
		s.getLogger().Printf("route fallback retry %s: candidate %q failed again (TRANSCRIBE_FAILED: %v); media.stt.on_route_error=fail, so the item fails and its fallback text leaves the index (SPEC §8.2.4)", doc.RelPath, cand, cause)
		return fallbackRetry{err: fmt.Errorf("%w: %w: transcribe %s on route %q: %w", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath, cand, cause)}
	}
	now := s.nowUTC()
	if v.ProducerChanged {
		def := s.defaultRoute()
		text, words, cov, err := s.transcribeStructuredWindowed(ctx, relPath, audio, def.stt)
		if err != nil {
			s.getLogger().Printf("route fallback retry %s: candidate %q failed again (%v) and the default profile %q failed too (TRANSCRIBE_FAILED: %v) (SPEC §8.2.4)", doc.RelPath, cand, cause, def.route, err)
			return fallbackRetry{err: fmt.Errorf("%w: %w: transcribe %s on the default profile %q: %w", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath, def.route, err)}
		}
		out := cachedTranscript{text: normalizeTranscriptNewlines(text), words: words, coverage: cov, route: &itemRoute{
			Language: rec.Language, Confidence: rec.Confidence, Route: def.route, FallbackFrom: cand,
			Uncovered: !routeCovers(def, rec.Language), FallbackAt: now.Format(time.RFC3339), FallbackBy: s.defaultRouteIdentity(),
		}}
		s.getLogger().Printf("route fallback retry %s: candidate %q failed again (TRANSCRIBE_FAILED: %v); the current default profile %q decoded it once (%s); next due %s (SPEC §8.2.4)", doc.RelPath, cand, cause, def.route, v.Why, s.nextDueLabel(now))
		return fallbackRetry{served: out, cache: &out}
	}
	out := hit
	rec.FallbackAt, rec.SkipReason = now.Format(time.RFC3339), ""
	out.route = &rec
	s.getLogger().Printf("route fallback retry %s: candidate %q failed again (TRANSCRIBE_FAILED: %v); the cached fallback text stays; next due %s (SPEC §8.2.4)", doc.RelPath, cand, cause, s.nextDueLabel(now))
	return fallbackRetry{served: out, cache: &out}
}

// retryCandidate is the route that decodes a fallback result again: the
// candidate the fallback record names when the current configuration still
// lists it for the language, else the first eligible candidate the current
// configuration selects. A language with no candidate has no route to retry.
func (s *Service) retryCandidate(lang, from string) (routedSTT, error) {
	cands, err := s.routeCandidates(lang)
	if err != nil {
		return routedSTT{}, err
	}
	for _, c := range cands {
		if c.route == from {
			return c, nil
		}
	}
	if len(cands) == 0 {
		return routedSTT{}, fmt.Errorf("no route candidate for language %q", lang)
	}
	return cands[0], nil
}

// nextDueLabel names when a fallback result that failed its retry at now is
// due again, for the retry log line.
func (s *Service) nextDueLabel(now time.Time) string {
	if s.routeRetryAfter <= 0 {
		return "on the next run"
	}
	return now.Add(s.routeRetryAfter).Format(time.RFC3339)
}

// readTrackTranscriptOrRetire is readTrackTranscript plus the §8.2.4 retirement:
// when a fallback result's route failed again and its text cannot be served,
// the track's earlier transcripts (and their translations and chunks) are
// retired before the failure is reported, as §8.6.13 retires a refused
// transcript. A retirement that fails is reported instead, because the stale
// text is still live.
func (s *Service) readTrackTranscriptOrRetire(ctx context.Context, doc model.Document, content []byte, tc trackContext) (string, []model.TimedWord, *TranscriptCoverage, *itemRoute, error) {
	text, words, coverage, route, err := s.readTrackTranscript(ctx, doc, content, tc)
	if err == nil || !errors.Is(err, errRouteFallbackRetired) {
		return text, words, coverage, route, err
	}
	if rerr := s.retireTrackTranscripts(ctx, doc, tc); rerr != nil {
		return "", nil, nil, nil, fmt.Errorf("retire route-fallback transcript of %s: %w (after: %w)", doc.RelPath, rerr, err)
	}
	return "", nil, nil, nil, err
}
