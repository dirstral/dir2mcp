package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/langdetect"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// This file implements SPEC §8.2.2 (dir2mcp #1029): per-window language
// identification, routing and honest coverage for a recording that changes
// language inside itself.
//
// §8.2.1 resolves ONE source language per item and routes the whole item on it.
// A decoder committed to the wrong language does not fail: it emits the nearest
// in-vocabulary language, so the minority passages come out in the wrong script
// or are dropped, and the item-level floor cannot see it because the item's
// resolved language IS covered. Under media.stt.language_scope: window the same
// three steps (resolve, route, floor) run per decode window instead.
//
// No audio-side language detector exists in this repository, and none is added.
// The window is identified by the decoder that heard it: a Whisper-class server
// reports the language it decoded under (TranscriptResult.Language), with its
// probability where the server exposes one. A provider that reports none falls
// back to the text detector on the window's own transcript. When the resolved
// language routes to another STT profile, the window is decoded again on that
// profile, so the extra request is paid only for windows that actually move.

// languageScopeItem / languageScopeWindow are the resolved values of
// media.stt.language_scope (SPEC §8.2.2). Only "window" (case-insensitive)
// selects the per-window contract; everything else, including empty and any
// unrecognized value, is "item", the §8.2.1 behaviour, so a Service built from
// an unvalidated config still decodes exactly as before (config.Validate rejects
// unknown values).
const (
	languageScopeItem   = "item"
	languageScopeWindow = "window"
)

// windowLanguageMinConfidence is the confidence floor for a language the
// DECODER reported for a window (§8.2.2 "confidence floor and continuity"). A
// report below it, or no report, makes the window inherit the preceding window's
// language. A decoder that reports a language but no confidence (0) is trusted:
// it heard the audio, and treating "no number" as "low number" would make every
// such window inherit and never start a new language range. The text-detector
// fallback keeps its own floor, langdetect.DefaultMinConfidence.
const windowLanguageMinConfidence = 0.5

func normalizeLanguageScope(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), languageScopeWindow) {
		return languageScopeWindow
	}
	return languageScopeItem
}

// SetLanguageScope overrides the resolved media.stt.language_scope, primarily
// for tests that need the per-window contract without threading a full config.
func (s *Service) SetLanguageScope(scope string) {
	s.languageScope = normalizeLanguageScope(scope)
}

// windowScoped reports whether the §8.2.2 per-window contract is active.
func (s *Service) windowScoped() bool {
	return s.languageScope == languageScopeWindow
}

// routedSTT is a transcriber bound to one STT provider profile: the transcriber
// itself, the profile name recorded as coverage.languages[].route, and the
// profile's declared coverage (stt_languages) the per-window floor is evaluated
// against.
type routedSTT struct {
	stt      model.Transcriber
	route    string
	coverage []string
}

// defaultRoute is the STT profile the item would have used under §8.2.1: the
// configured transcriber, its resolved profile name and declared coverage.
func (s *Service) defaultRoute() routedSTT {
	return routedSTT{stt: s.transcriber, route: s.sttProvider, coverage: s.sttLanguages}
}

// SetRouteTranscriber installs a transcriber for one language route, for tests
// that exercise per-window routing without live provider profiles. lang is
// matched on its BCP-47 primary subtag; route is the name recorded on the
// coverage entry; coverage is the route's declared stt_languages.
func (s *Service) SetRouteTranscriber(lang string, tr model.Transcriber, route string, coverage []string) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routes == nil {
		s.routes = map[string]routedSTT{}
	}
	s.routes[provider.PrimarySubtag(lang)] = routedSTT{stt: tr, route: route, coverage: append([]string(nil), coverage...)}
}

// routeForLanguage returns the STT route for a resolved window language
// (§8.2.2 "per-window routing"): the media.stt.language_providers target when
// the language matches a key, else the default route. Built routes are cached
// for the life of the Service, so a long recording that alternates between two
// languages builds each transcriber once. An unknown language (empty) always
// takes the default route.
func (s *Service) routeForLanguage(lang string) (routedSTT, error) {
	key := provider.PrimarySubtag(lang)
	if key == "" {
		return s.defaultRoute(), nil
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if r, ok := s.routes[key]; ok {
		return r, nil
	}
	mapped, ok := s.cfg.MediaSTTLanguageProviders[key]
	if !ok {
		return s.defaultRoute(), nil
	}
	prof, err := s.cfg.Providers().ResolveExplicit(provider.CapSTT, mapped, true)
	if err != nil {
		return routedSTT{}, fmt.Errorf("route language %q to STT profile %q: %w", key, mapped, err)
	}
	// The same profile adjustments TranscriberFromConfigWithLanguage applies to
	// the default route, so a routed window decodes under the same VAD and
	// request limits as an unrouted one; only the model differs.
	prof.STTLanguage = key
	prof.STTVAD = s.cfg.MediaVAD
	prof.STTMaxPayloadMB = s.cfg.MediaSTTMaxPayloadMB
	prof.STTRequestTimeoutSec = s.cfg.MediaSTTRequestTimeoutSec
	tr, err := buildTranscriber(prof)
	if err != nil {
		return routedSTT{}, fmt.Errorf("build STT route %q for language %q: %w", mapped, key, err)
	}
	r := routedSTT{stt: tr, route: strings.TrimSpace(prof.Name), coverage: append([]string(nil), prof.STTLanguages...)}
	if s.routes == nil {
		s.routes = map[string]routedSTT{}
	}
	s.routes[key] = r
	return r, nil
}

// windowLanguageState is the continuity the §8.2.2 rules carry from one window
// to the next: the language the preceding window resolved to (empty = unknown),
// how it was obtained, its confidence when detected, whether a pin fixed it for
// every window, and the route that decoded the preceding window.
type windowLanguageState struct {
	lang   string
	source string
	conf   *float64
	pinned bool
	route  routedSTT
}

// newWindowLanguageState seeds the state with the item-level resolution of
// §8.2.1: an operator pin applies to every window and disables detection
// (§8.2.2 "per-window resolution"); otherwise the first window starts unknown
// and detects. The route starts as the default profile.
func (s *Service) newWindowLanguageState() *windowLanguageState {
	st := &windowLanguageState{route: s.defaultRoute()}
	if pin := provider.PrimarySubtag(s.transcriptLanguage); pin != "" {
		st.lang, st.source, st.pinned = pin, langSourceConfigured, true
	}
	return st
}

// pieceResult is one decoded piece of a window and the result it produced.
type pieceResult struct {
	piece windowPiece
	res   model.TranscriptResult
}

// windowDecode is what one scoped window produced: the decoded pieces (empty when
// the window failed or was refused), the coverage ranges they cover, the
// coverage.languages entries for the window, and the refused range when the
// window was refused.
type windowDecode struct {
	decoded   []TranscriptWindow
	covered   []CoverageRange
	languages []model.CoverageLanguage
	refused   []model.RefusedRange
	err       error
}

// decodeWindowScoped decodes one scheduled window under the §8.2.2 rules.
// core is the stretch of the recording this window is responsible for in the
// merged transcript (its start through the next window's start, or the end of
// the recording for the last window); coverage.languages entries are clipped to
// it so consecutive windows never overlap in the record.
//
// Order of operations, each one a rule in §8.2.2:
//  1. decode the pieces on the CURRENT route (the preceding window's);
//  2. identify the window's language from the decoder's report, else from the
//     text, else inherit the preceding window's (or stay unknown);
//  3. resolve the route for that language; when it differs from the route that
//     decoded, decode again on it (the only extra request this scope costs);
//  4. evaluate the honest-coverage floor against the decoding route: warn keeps
//     the text and records covered=false, skip refuses the window;
//  5. record one coverage.languages entry per decoded piece and advance the
//     state.
func (s *Service) decodeWindowScoped(ctx context.Context, relPath string, pieces []windowPiece, plan windowSchedule, core CoverageRange, st *windowLanguageState) windowDecode {
	var out windowDecode
	cur := st.route
	results, err := s.decodePieces(ctx, relPath, cur.stt, pieces, plan)
	out.err = err
	if len(results) == 0 {
		return out
	}
	lang, source, conf := st.resolve(results)
	results, cur = s.rerouteWindow(ctx, relPath, pieces, plan, core, results, cur, lang)
	covered := routeCovers(cur, lang)
	if !covered && s.onUncoveredLanguage == onUncoveredLanguageSkip {
		s.getLogger().Printf("windowed %s: refusing window [%d,%d]ms of %s: language %q is outside %s's declared coverage %v (media.stt.on_uncovered_language=skip, SPEC §8.2.2)",
			plan.label, core.StartMS, core.EndMS, relPath, lang, cur.route, cur.coverage)
		out.refused = []model.RefusedRange{{StartMS: core.StartMS, EndMS: core.EndMS, Reason: model.RefusedLanguageUncovered}}
		st.advance(lang, source, conf, cur)
		return out
	}
	if !covered {
		s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s is in %q, outside %s's declared coverage %v; indexed anyway and recorded covered=false (SPEC §8.2.2)",
			plan.label, core.StartMS, core.EndMS, relPath, lang, cur.route, cur.coverage)
	}
	out.decoded, out.covered, out.languages = recordWindow(results, core, lang, source, conf, cur, covered)
	st.advance(lang, source, conf, cur)
	return out
}

// resolve identifies the window's language under the §8.2.2 resolution and
// continuity rules: a pin fixes it for every window; else the decoder's report
// or the text detector; else the preceding window's language, recorded as
// inherited; else unknown, with no tag invented.
func (st *windowLanguageState) resolve(results []pieceResult) (lang, source string, conf *float64) {
	if st.pinned {
		return st.lang, langSourceConfigured, nil
	}
	if tag, c, ok := identifyWindowLanguage(results); ok {
		if c > 0 {
			cc := c
			return tag, langSourceDetected, &cc
		}
		return tag, langSourceDetected, nil
	}
	if st.lang != "" {
		// Below the floor, or nothing reported: the preceding window's language
		// carries over and says so. Confidence is that window's, not this one's,
		// so it is not repeated.
		return st.lang, model.CoverageLanguageSourceInherited, nil
	}
	// Unknown, and nothing to inherit (§8.2.2 "unknown language"): the default
	// route decodes it and the floor does not apply.
	return "", "", nil
}

// rerouteWindow applies the §8.2.2 per-window routing rule: when the resolved
// language maps to a profile other than the one that decoded the window, the
// pieces are decoded again on that profile and its results replace the first
// decode. When the routed profile produces nothing, or the route cannot be
// built, the first decode stands and its route is what the record names.
func (s *Service) rerouteWindow(ctx context.Context, relPath string, pieces []windowPiece, plan windowSchedule, core CoverageRange, results []pieceResult, cur routedSTT, lang string) ([]pieceResult, routedSTT) {
	want, err := s.routeForLanguage(lang)
	if err != nil {
		// A route that cannot be built is a configuration defect the item-level
		// path would have reported at construction. Keep the text the current
		// route produced, record it honestly under that route, and say why.
		s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s stays on route %q: %v", plan.label, core.StartMS, core.EndMS, relPath, cur.route, err)
		return results, cur
	}
	if want.route == cur.route {
		return results, cur
	}
	rerouted, derr := s.decodePieces(ctx, relPath, want.stt, pieces, plan)
	if len(rerouted) == 0 {
		s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s resolved to %q but route %q returned nothing (%v); keeping the %q decode",
			plan.label, core.StartMS, core.EndMS, relPath, lang, want.route, derr, cur.route)
		return results, cur
	}
	return rerouted, want
}

// routeCovers evaluates the §8.2.1 floor for one window against the profile
// that decoded it: false only when the profile declares a coverage set and the
// resolved language is outside it. An unknown language is never uncovered.
func routeCovers(route routedSTT, lang string) bool {
	if lang == "" {
		return true
	}
	declared, covered := provider.STTLanguageCoverageSet(route.coverage, lang)
	return !declared || covered
}

// recordWindow turns the decoded pieces of one window into merge input, coverage
// ranges and one coverage.languages entry per piece, clipped to the core.
func recordWindow(results []pieceResult, core CoverageRange, lang, source string, conf *float64, route routedSTT, covered bool) ([]TranscriptWindow, []CoverageRange, []model.CoverageLanguage) {
	var decoded []TranscriptWindow
	var ranges []CoverageRange
	var entries []model.CoverageLanguage
	for _, r := range results {
		decoded = append(decoded, TranscriptWindow{StartMS: r.piece.startMS, Res: r.res})
		ranges = append(ranges, CoverageRange{StartMS: r.piece.startMS, EndMS: r.piece.endMS})
		start, end := clipToCore(r.piece.startMS, r.piece.endMS, core)
		if end <= start {
			continue
		}
		entry := model.CoverageLanguage{StartMS: start, EndMS: end, Language: lang, LanguageSource: source, Route: route.route, Covered: covered}
		if source == langSourceDetected && conf != nil {
			c := *conf
			entry.LanguageConfidence = &c
		}
		entries = append(entries, entry)
	}
	return decoded, ranges, entries
}

// advance moves the continuity state to what this window resolved. An inherited
// window keeps the ORIGINAL source's confidence out of the state, so a later
// inheriting window does not repeat a number that was never measured for it.
func (st *windowLanguageState) advance(lang, source string, conf *float64, route routedSTT) {
	if st.pinned {
		st.route = route
		return
	}
	st.lang, st.source, st.conf, st.route = lang, source, conf, route
	if source == model.CoverageLanguageSourceInherited {
		st.conf = nil
	}
}

// clipToCore intersects a piece range with the window's core.
func clipToCore(startMS, endMS int, core CoverageRange) (int, int) {
	if startMS < core.StartMS {
		startMS = core.StartMS
	}
	if endMS > core.EndMS {
		endMS = core.EndMS
	}
	return startMS, endMS
}

// decodePieces decodes every piece of a window on one transcriber, keeping the
// pieces that produced content, and returns the first failure. It is
// decodeWindowPieces with the full TranscriptResult kept, because the §8.2.2
// identification reads the language the decoder reported.
func (s *Service) decodePieces(ctx context.Context, relPath string, stt model.Transcriber, pieces []windowPiece, plan windowSchedule) ([]pieceResult, error) {
	var out []pieceResult
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
		res, err := s.transcribeResult(ctx, model.TranscriberForAudioDuration(stt, p.endMS-p.startMS), relPath, p.data)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.getLogger().Printf("windowed %s: skip window [%d,%d]ms of %s: %v", plan.label, p.startMS, p.endMS, relPath, err)
			continue
		}
		out = append(out, pieceResult{piece: p, res: res})
	}
	return out, firstErr
}

// transcribeResult is transcribeWith keeping the whole TranscriptResult, so the
// caller sees the language the decoder reported. A text-only transcriber yields
// no words and no language.
func (s *Service) transcribeResult(ctx context.Context, stt model.Transcriber, relPath string, content []byte) (model.TranscriptResult, error) {
	if st, ok := stt.(model.StructuredTranscriber); ok {
		return st.TranscribeStructured(ctx, relPath, content)
	}
	text, err := stt.Transcribe(ctx, relPath, content)
	if err != nil {
		return model.TranscriptResult{}, err
	}
	return model.TranscriptResult{Text: text}, nil
}

// identifyWindowLanguage resolves a window's detected language from what its
// pieces returned (§8.2.2 "per-window resolution"). The decoder's own report
// wins: the first piece that names a language names the window, and the lowest
// reported confidence across the pieces is the window's; a report below the
// floor is declined. With no report, the text detector runs on the window's own
// transcript under its default floor. ok is false when nothing resolved, which
// is the caller's cue to inherit.
func identifyWindowLanguage(results []pieceResult) (lang string, conf float64, ok bool) {
	var texts []string
	reported := ""
	minConf := -1.0
	for _, r := range results {
		texts = append(texts, r.res.Text)
		tag := provider.PrimarySubtag(r.res.Language)
		if tag == "" {
			continue
		}
		if reported == "" {
			reported = tag
		}
		if r.res.LanguageConfidence > 0 && (minConf < 0 || r.res.LanguageConfidence < minConf) {
			minConf = r.res.LanguageConfidence
		}
	}
	if reported != "" {
		if minConf < 0 {
			// Reported without a confidence: trusted, see windowLanguageMinConfidence.
			return reported, 0, true
		}
		if minConf >= windowLanguageMinConfidence {
			return reported, minConf, true
		}
		return "", minConf, false
	}
	text := stripSegmentMarkers(strings.Join(texts, "\n"))
	tag, c, detected := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	if !detected {
		return "", c, false
	}
	return tag, c, true
}

// stripSegmentMarkers removes the leading `[mm:ss]` markers of a
// segment-formatted transcript so the text detector sees words, not clock
// digits that would dilute its letter count.
func stripSegmentMarkers(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			if i := strings.Index(line, "]"); i > 0 {
				line = strings.TrimSpace(line[i+1:])
			}
		}
		if line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// coalesceCoverageLanguages sorts the per-piece entries by start and merges
// every adjacent or overlapping pair whose (language, language_source, route,
// covered) are identical (§8.2.2 "recording"), keeping the minimum confidence.
// A later entry that starts inside an earlier one of a DIFFERENT key is clipped
// to start where the earlier ends, so the record is non-overlapping and
// ascending as the spec requires. Empty and inverted entries are dropped.
func coalesceCoverageLanguages(entries []model.CoverageLanguage, totalMS int) []model.CoverageLanguage {
	cleaned := make([]model.CoverageLanguage, 0, len(entries))
	for _, e := range entries {
		if e.StartMS < 0 {
			e.StartMS = 0
		}
		if totalMS > 0 && e.EndMS > totalMS {
			e.EndMS = totalMS
		}
		if e.EndMS > e.StartMS {
			cleaned = append(cleaned, e)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	sort.SliceStable(cleaned, func(a, b int) bool {
		if cleaned[a].StartMS != cleaned[b].StartMS {
			return cleaned[a].StartMS < cleaned[b].StartMS
		}
		return cleaned[a].EndMS < cleaned[b].EndMS
	})
	out := []model.CoverageLanguage{cleaned[0]}
	for _, e := range cleaned[1:] {
		last := &out[len(out)-1]
		if sameLanguageEntry(*last, e) && e.StartMS <= last.EndMS {
			if e.EndMS > last.EndMS {
				last.EndMS = e.EndMS
			}
			last.LanguageConfidence = minConfidence(last.LanguageConfidence, e.LanguageConfidence)
			continue
		}
		if e.StartMS < last.EndMS {
			e.StartMS = last.EndMS
			if e.EndMS <= e.StartMS {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func sameLanguageEntry(a, b model.CoverageLanguage) bool {
	return a.Language == b.Language && a.LanguageSource == b.LanguageSource && a.Route == b.Route && a.Covered == b.Covered
}

func minConfidence(a, b *float64) *float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *b < *a {
		return b
	}
	return a
}

// coalesceRefusedRanges sorts refused ranges and merges adjacent or overlapping
// ones with the same reason.
func coalesceRefusedRanges(ranges []model.RefusedRange, totalMS int) []model.RefusedRange {
	cleaned := make([]model.RefusedRange, 0, len(ranges))
	for _, r := range ranges {
		if r.StartMS < 0 {
			r.StartMS = 0
		}
		if totalMS > 0 && r.EndMS > totalMS {
			r.EndMS = totalMS
		}
		if r.EndMS > r.StartMS {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	sort.SliceStable(cleaned, func(a, b int) bool {
		if cleaned[a].StartMS != cleaned[b].StartMS {
			return cleaned[a].StartMS < cleaned[b].StartMS
		}
		return cleaned[a].EndMS < cleaned[b].EndMS
	})
	out := []model.RefusedRange{cleaned[0]}
	for _, r := range cleaned[1:] {
		last := &out[len(out)-1]
		if r.Reason == last.Reason && r.StartMS <= last.EndMS {
			if r.EndMS > last.EndMS {
				last.EndMS = r.EndMS
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// representationLanguage picks the representation-level language of a
// window-scoped transcript (§8.2.2 "recording"): the language with the largest
// summed duration among COVERED entries, ties to the earliest start. ok is false
// when no covered entry carries a language, in which case the caller keeps the
// item-level resolution. conf is the smallest confidence recorded for the chosen
// language's detected entries, nil when none was recorded.
func representationLanguage(entries []model.CoverageLanguage) (lang string, conf *float64, ok bool) {
	type acc struct {
		ms    int
		first int
		conf  *float64
	}
	sums := map[string]*acc{}
	for _, e := range entries {
		if !e.Covered || e.Language == "" {
			continue
		}
		a, exists := sums[e.Language]
		if !exists {
			a = &acc{first: e.StartMS}
			sums[e.Language] = a
		}
		a.ms += e.EndMS - e.StartMS
		if e.StartMS < a.first {
			a.first = e.StartMS
		}
		if e.LanguageSource == langSourceDetected {
			a.conf = minConfidence(a.conf, e.LanguageConfidence)
		}
	}
	best := ""
	for l, a := range sums {
		if best == "" {
			best = l
			continue
		}
		b := sums[best]
		if a.ms > b.ms || (a.ms == b.ms && a.first < b.first) {
			best = l
		}
	}
	if best == "" {
		return "", nil, false
	}
	return best, sums[best].conf, true
}

// anyUncovered reports whether any entry records covered=false: the §8.2.1
// covered fact on the transcript is true only when every decoded range is
// covered (§8.2.2).
func anyUncovered(entries []model.CoverageLanguage) bool {
	for _, e := range entries {
		if !e.Covered {
			return true
		}
	}
	return false
}

// stampSegmentLanguages marks every transcript segment whose window resolved to
// a language other than the representation's (§8.2.2 "recording": a segment span
// in another language records `language` in its extra_json). A segment belongs
// to the entry that contains its start; an unmarked segment is in the
// representation's language. It runs before the chunk window merges segments,
// so the fourth close rule sees the marks.
func stampSegmentLanguages(segs []chunkSegment, entries []model.CoverageLanguage, repLang string) {
	if len(entries) == 0 {
		return
	}
	repLang = provider.PrimarySubtag(repLang)
	for i := range segs {
		sp := &segs[i].Span
		if !strings.EqualFold(strings.TrimSpace(sp.Kind), "time") {
			continue
		}
		for i, e := range entries {
			// A segment belongs to the entry that contains its start. The final
			// entry also owns a segment that starts exactly at the recording's
			// end, which a provider emits for the last breath group.
			last := i == len(entries)-1
			if sp.StartMS >= e.StartMS && (sp.StartMS < e.EndMS || (last && sp.StartMS == e.EndMS)) {
				if e.Language != "" && e.Language != repLang {
					sp.Language = e.Language
				}
				break
			}
		}
	}
}

// allWindowsRefusedReason implements the §8.2.2 terminal-status rule for a
// decode in which NO window produced persisted text and at least one was
// refused: language_uncovered when any refusal was for language (an operator
// decision that recurs every run), else quality_gate. Empty when nothing was
// refused, so an ordinary empty or failed decode is judged as before.
func allWindowsRefusedReason(coverage *TranscriptCoverage) string {
	if coverage == nil || coverage.WindowsDecoded > 0 || len(coverage.Refused) == 0 {
		return ""
	}
	reason := model.RefusedQualityGate
	for _, r := range coverage.Refused {
		if r.Reason == model.RefusedLanguageUncovered {
			return model.RefusedLanguageUncovered
		}
	}
	return reason
}

// languageScopeIdentity renders the §8.2.2 component of the transcript
// derivation identity (§8.6.7): empty under item scope, so every existing
// corpus's identity is byte-stable, and "scope=window;routes=<lang>=<profile>,..."
// with the routes sorted under window scope. Both the scope and the route table
// change which model decodes which audio, and therefore the text.
func languageScopeIdentity(scope string, routes map[string]string) string {
	if normalizeLanguageScope(scope) != languageScopeWindow {
		return ""
	}
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, provider.PrimarySubtag(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		parts = append(parts, k+"="+strings.TrimSpace(routes[k]))
	}
	return "scope=window;routes=" + strings.Join(parts, ",")
}

// renderLanguageRoutes is the canonical string form of media.stt.language_providers
// recorded on a window-scoped transcript's meta_json (language_routes), so the
// recorded identity can be rebuilt with languageScopeIdentity on the next run.
func renderLanguageRoutes(routes map[string]string) string {
	id := languageScopeIdentity(languageScopeWindow, routes)
	return strings.TrimPrefix(id, "scope=window;routes=")
}

// parseLanguageRoutes inverts renderLanguageRoutes.
func parseLanguageRoutes(rendered string) map[string]string {
	rendered = strings.TrimSpace(rendered)
	if rendered == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(rendered, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// coverageLanguages returns the §8.2.2 language entries of a coverage record,
// nil for a nil record or an item-scoped one.
func coverageLanguages(coverage *TranscriptCoverage) []model.CoverageLanguage {
	if coverage == nil {
		return nil
	}
	return coverage.Languages
}

// windowLanguageForMeta resolves the representation-level language of a
// window-scoped transcript (§8.2.2 "recording"): the largest covered language
// by duration, with its §8.8 source (configured when an operator pin fixed
// every window, detected otherwise) and the smallest confidence recorded for
// it. ok is false under item scope, or when no covered entry carries a
// language, and the caller keeps today's item-level resolution.
func windowLanguageForMeta(coverage *TranscriptCoverage, pin string) (lang string, conf *float64, ok bool) {
	entries := coverageLanguages(coverage)
	if len(entries) == 0 {
		return "", nil, false
	}
	lang, conf, ok = representationLanguage(entries)
	if !ok {
		return "", nil, false
	}
	if provider.PrimarySubtag(pin) != "" {
		return lang, nil, true
	}
	return lang, conf, true
}

// applyWindowLanguageMeta rewrites the language fields of a window-scoped
// transcript's meta from its coverage record (§8.2.2): the representation
// language and its source, language_covered=false when any decoded range was
// uncovered, and the scope and route table that join the derivation identity.
// It leaves an item-scoped meta untouched.
func (s *Service) applyWindowLanguageMeta(meta *transcriptMeta, coverage *TranscriptCoverage) {
	if !s.windowScoped() || coverage == nil {
		return
	}
	meta.LanguageScope = languageScopeWindow
	meta.LanguageRoutes = renderLanguageRoutes(s.cfg.MediaSTTLanguageProviders)
	lang, conf, ok := windowLanguageForMeta(coverage, s.transcriptLanguage)
	if ok {
		meta.Language = lang
		if provider.PrimarySubtag(s.transcriptLanguage) != "" {
			meta.LanguageSource = langSourceConfigured
			meta.LanguageConfidence = nil
		} else {
			meta.LanguageSource = langSourceDetected
			meta.LanguageConfidence = conf
		}
	}
	// The §8.2.1 covered fact is true only when every decoded range is covered.
	// It is set only to false (absence is "no assertion"), as sttTranscriptMeta
	// does for the item-level check.
	if anyUncovered(coverage.Languages) {
		no := false
		meta.LanguageCovered = &no
	} else if ok {
		meta.LanguageCovered = nil
	}
}
