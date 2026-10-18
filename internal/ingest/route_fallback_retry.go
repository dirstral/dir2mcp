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

// SetRouteRetryAfter overrides media.stt.route_retry_after, for tests.
func (s *Service) SetRouteRetryAfter(d time.Duration) {
	s.routeRetryAfter = d
}

// SetClock overrides the time source of the route-fallback retry rule, for
// tests. Nil restores time.Now.
func (s *Service) SetClock(now func() time.Time) {
	s.clock = now
}

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
	if coverageHasFallback(c.coverage) {
		return c.coverage.RouteFallbackAt, c.coverage.RouteFallbackBy, true
	}
	return "", "", false
}

// fallbackVerdict is the §8.2.4 decision on one fallback result: whether it is
// decoded again on this run, whether the default profile that produced it
// changed (the result is then stale under both policies), and why, for the log.
type fallbackVerdict struct {
	due             bool
	producerChanged bool
	why             string
}

// routeFallbackVerdict applies the §8.2.4 rules to a fallback result's stamp.
func (s *Service) routeFallbackVerdict(at, by string) fallbackVerdict {
	if strings.TrimSpace(by) == "" || strings.TrimSpace(by) != s.defaultRouteIdentity() {
		return fallbackVerdict{due: true, producerChanged: true, why: fmt.Sprintf("route_fallback_by %q is not the current default profile %q", by, s.defaultRouteIdentity())}
	}
	if !s.routeErrorFallback {
		return fallbackVerdict{due: true, why: "media.stt.on_route_error=fail"}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		return fallbackVerdict{due: true, why: "no valid route_fallback_at"}
	}
	if s.routeRetryAfter <= 0 {
		return fallbackVerdict{due: true, why: "media.stt.route_retry_after=0"}
	}
	if s.nowUTC().Before(t.Add(s.routeRetryAfter)) {
		return fallbackVerdict{}
	}
	return fallbackVerdict{due: true, why: "route_retry_after elapsed"}
}

// routeFallbackMeta is the part of a transcript meta_json the §8.2.4 gate reads.
type routeFallbackMeta struct {
	RouteFallbackFrom string              `json:"route_fallback_from"`
	RouteFallbackAt   string              `json:"route_fallback_at"`
	RouteFallbackBy   string              `json:"route_fallback_by"`
	Coverage          *TranscriptCoverage `json:"coverage"`
}

// RouteFallbackDue reports whether a stored transcript meta_json holds a
// fallback result that this run must decode again (SPEC §8.2.4), and why. A
// meta_json with no fallback record, or one that does not parse, is never due.
func (s *Service) RouteFallbackDue(metaJSON string) (bool, string) {
	var meta routeFallbackMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return false, ""
	}
	if meta.RouteFallbackFrom == "" && !coverageHasFallback(meta.Coverage) {
		return false, ""
	}
	v := s.routeFallbackVerdict(meta.RouteFallbackAt, meta.RouteFallbackBy)
	return v.due, v.why
}

// routeFallbackStale is the §8.2.4 half of the §8.6.7 re-derivation gate: an
// unchanged document whose stored transcript holds a due fallback result is
// processed again, so its route decodes it. A result that is not due costs
// nothing. Every source transcript track is checked: an additional audio track
// (§8.6.12) is its own decode with its own fallback record.
func (s *Service) routeFallbackStale(ctx context.Context, reader representationMetaReader, relPath string) bool {
	if s.transcriber == nil {
		return false
	}
	for _, t := range s.sourceTranscriptMetas(ctx, reader, relPath) {
		if due, why := s.RouteFallbackDue(t.meta); due {
			s.getLogger().Printf("route fallback: %s (%s) holds a route-fallback transcript (%s); decoding it again on its route this scan (SPEC §8.2.4)", relPath, t.repType, why)
			return true
		}
	}
	return false
}

// transcriptMetaRow is one stored source transcript's rep_type and meta_json.
type transcriptMetaRow struct {
	repType, meta string
}

// sourceTranscriptMetas returns the meta_json of every live source transcript
// of a document: the bare transcript and each additional track's
// transcript@t<N>, without translations. A store that cannot list its
// representations yields the bare transcript only.
func (s *Service) sourceTranscriptMetas(ctx context.Context, reader representationMetaReader, relPath string) []transcriptMetaRow {
	lister, ok := s.store.(activeRepresentationLister)
	if !ok {
		meta, err := reader.RepresentationMetaByType(ctx, relPath, RepTypeTranscript)
		if err != nil || strings.TrimSpace(meta) == "" {
			return nil
		}
		return []transcriptMetaRow{{repType: RepTypeTranscript, meta: meta}}
	}
	reps, err := lister.ActiveRepresentations(ctx, relPath)
	if err != nil {
		return nil
	}
	var out []transcriptMetaRow
	for _, rep := range reps {
		isSource := rep.RepType == RepTypeTranscript ||
			(strings.HasPrefix(rep.RepType, RepTypeTranscript+"@") && !strings.Contains(rep.RepType, "-"))
		if isSource && strings.TrimSpace(rep.MetaJSON) != "" {
			out = append(out, transcriptMetaRow{repType: rep.RepType, meta: rep.MetaJSON})
		}
	}
	return out
}

// fallbackRetry is the outcome of one §8.2.4 retry: the transcript the index
// gets, the entry the cache gets (nil leaves the cache entry as it is), and
// the item's failure.
type fallbackRetry struct {
	served cachedTranscript
	cache  *cachedTranscript
	err    error
}

// serveCachedTranscript returns a cached transcript, after the §8.2.4 retry
// when it is a fallback result that is due.
func (s *Service) serveCachedTranscript(ctx context.Context, doc model.Document, content []byte, paths transcriptCachePaths, hit cachedTranscript) (string, []model.TimedWord, *TranscriptCoverage, *itemRoute, error) {
	at, by, ok := hit.fallbackStamp()
	if !ok {
		return hit.unpack()
	}
	v := s.routeFallbackVerdict(at, by)
	if !v.due {
		return hit.unpack()
	}
	var r fallbackRetry
	if hit.route != nil && hit.route.FallbackFrom != "" {
		r = s.retryItemFallback(ctx, doc, content, hit, v)
	} else {
		r = s.retryWindowFallback(ctx, doc, content, hit, v)
	}
	if r.cache != nil {
		if err := s.publishTranscriptCache(paths, *r.cache); err != nil {
			s.getLogger().Printf("route fallback retry %s: %v; the next run reads the earlier cache entry", doc.RelPath, err)
		}
	}
	if r.err != nil {
		return "", nil, nil, nil, r.err
	}
	return r.served.unpack()
}

// retryItemFallback is the §8.2.4 retry under item scope: the item is decoded
// once on the route for its recorded language. The identifier is not asked
// again: its binding and probe are in the identity, so they are unchanged.
func (s *Service) retryItemFallback(ctx context.Context, doc model.Document, content []byte, hit cachedTranscript, v fallbackVerdict) fallbackRetry {
	rec := *hit.route
	relPath, audio, ok, err := s.transcriptInput(ctx, doc, content)
	if err == nil && !ok {
		err = errors.New("no audio track to decode")
	}
	cand := rec.FallbackFrom
	var text string
	var words []model.TimedWord
	var cov *TranscriptCoverage
	if err == nil {
		var route routedSTT
		route, err = s.retryCandidate(rec.Language, rec.FallbackFrom)
		if err == nil {
			cand = route.route
			text, words, cov, err = s.transcribeStructuredWindowed(ctx, relPath, audio, route.stt)
		}
		if err == nil {
			out := cachedTranscript{text: normalizeTranscriptNewlines(text), words: words, coverage: cov,
				route: &itemRoute{Language: rec.Language, Confidence: rec.Confidence, Route: route.route, Uncovered: !routeCovers(route, rec.Language)}}
			s.getLogger().Printf("route fallback retry %s: candidate %q recovered; its text replaces the fallback text (%s, SPEC §8.2.4)", doc.RelPath, cand, v.why)
			return fallbackRetry{served: out, cache: &out}
		}
	}
	return s.itemRetryFailed(ctx, doc, relPath, audio, hit, cand, v, err)
}

// itemRetryFailed settles an item whose route failed again (SPEC §8.2.4):
// under fail the item fails and its fallback text is retired; under default a
// changed producer decodes it once on the new default profile; otherwise the
// cached fallback text stays and route_fallback_at moves to now.
func (s *Service) itemRetryFailed(ctx context.Context, doc model.Document, relPath string, audio []byte, hit cachedTranscript, cand string, v fallbackVerdict, cause error) fallbackRetry {
	rec := *hit.route
	if !s.routeErrorFallback {
		s.getLogger().Printf("route fallback retry %s: candidate %q failed again (TRANSCRIBE_FAILED: %v); media.stt.on_route_error=fail, so the item fails and its fallback text leaves the index (SPEC §8.2.4)", doc.RelPath, cand, cause)
		return fallbackRetry{err: fmt.Errorf("%w: %w: transcribe %s on route %q: %w", ErrTranscriptProviderFailure, errRouteFallbackRetired, doc.RelPath, cand, cause)}
	}
	now := s.nowUTC()
	if v.producerChanged {
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
		s.getLogger().Printf("route fallback retry %s: candidate %q failed again (TRANSCRIBE_FAILED: %v); the current default profile %q decoded it once (%s); next due %s (SPEC §8.2.4)", doc.RelPath, cand, cause, def.route, v.why, s.nextDueLabel(now))
		return fallbackRetry{served: out, cache: &out}
	}
	out := hit
	rec.FallbackAt = now.Format(time.RFC3339)
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

// RouteFallbackStaleForTest exposes the §8.2.4 re-derivation gate for one
// document, for tests in the tests/ tree.
func (s *Service) RouteFallbackStaleForTest(ctx context.Context, relPath string) bool {
	reader, ok := s.store.(representationMetaReader)
	return ok && s.routeFallbackStale(ctx, reader, relPath)
}
