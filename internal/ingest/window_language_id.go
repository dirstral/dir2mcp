package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/quality"
)

// This file implements SPEC §8.2.3 (dir2mcp #1031): an optional language
// identifier that outranks a decoder's own language report, ordered candidate
// routes a refused window falls through, and the validation records that decide
// which candidates are eligible. It applies under media.stt.language_scope:
// window; the item-scope identifier is a follow-up.

// identifierRouteKey is the key the identifier binding takes in the route
// table that joins the derivation identity. "@" cannot start a BCP-47 tag, so
// it never collides with a language route.
const identifierRouteKey = "@identifier"

// setupLanguageIdentifier builds the §8.2.3 identifier from its profile. A
// profile that cannot be built leaves the identifier off with a warning: the
// config was validated at load, and identification is advisory.
func (s *Service) setupLanguageIdentifier() {
	name := strings.TrimSpace(s.cfg.MediaSTTLanguageIdentifier)
	if name == "" {
		return
	}
	prof, err := s.cfg.Providers().ResolveExplicit(provider.CapSTT, name, true)
	if err != nil {
		s.getLogger().Printf("stt: media.stt.language_identifier %q is off: %v (SPEC §8.2.3)", name, err)
		return
	}
	prof.STTVAD = s.cfg.MediaVAD
	prof.STTMaxPayloadMB = s.cfg.MediaSTTMaxPayloadMB
	prof.STTRequestTimeoutSec = s.cfg.MediaSTTRequestTimeoutSec
	tr, err := buildTranscriber(prof)
	if err != nil {
		s.getLogger().Printf("stt: media.stt.language_identifier %q is off: %v (SPEC §8.2.3)", name, err)
		return
	}
	s.identifier = &routedSTT{stt: tr, route: strings.TrimSpace(prof.Name)}
}

// SetLanguageIdentifier installs an identifier, for tests. Nil turns it off.
func (s *Service) SetLanguageIdentifier(tr model.Transcriber, name string) {
	if tr == nil {
		s.identifier = nil
		return
	}
	s.identifier = &routedSTT{stt: tr, route: name}
}

// AddRouteCandidate appends a candidate to a language's route list, for tests
// that exercise §8.2.3 fall-through without live profiles.
func (s *Service) AddRouteCandidate(lang string, tr model.Transcriber, route string, coverage []string) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routeLists == nil {
		s.routeLists = map[string][]routedSTT{}
	}
	key := provider.PrimarySubtag(lang)
	s.routeLists[key] = append(s.routeLists[key], routedSTT{stt: tr, route: route, coverage: append([]string(nil), coverage...)})
}

// routeCandidates returns the eligible candidates for a resolved language, in
// preference order (SPEC §8.2.3), or nil when no route matches and the default
// profile decodes. Built lists are cached for the life of the Service.
func (s *Service) routeCandidates(lang string) ([]routedSTT, error) {
	key := provider.PrimarySubtag(lang)
	if key == "" {
		return nil, nil
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if list, ok := s.routeLists[key]; ok {
		return list, nil
	}
	if r, ok := s.routes[key]; ok {
		return []routedSTT{r}, nil
	}
	names := s.configuredCandidates(key)
	if len(names) == 0 {
		return nil, nil
	}
	var list []routedSTT
	for _, name := range names {
		prof, err := s.cfg.Providers().ResolveExplicit(provider.CapSTT, name, true)
		if err != nil {
			return nil, fmt.Errorf("route language %q to STT profile %q: %w", key, name, err)
		}
		if s.cfg.MediaSTTRequireValidation && !prof.ValidatedFor(key) {
			s.getLogger().Printf("stt: route candidate %q for %q skipped: no stt_validation record for the language (media.stt.require_validation, SPEC §8.2.3)", name, key)
			continue
		}
		r, err := s.buildRoute(prof, key)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if s.routeLists == nil {
		s.routeLists = map[string][]routedSTT{}
	}
	s.routeLists[key] = list
	return list, nil
}

// configuredCandidates is the configured ordered list for a language: the
// §8.2.3 list when the config carried one, else the single §8.2.1 name.
func (s *Service) configuredCandidates(key string) []string {
	if names := s.cfg.MediaSTTLanguageCandidates[key]; len(names) > 0 {
		return names
	}
	if name, ok := s.cfg.MediaSTTLanguageProviders[key]; ok {
		return []string{name}
	}
	return nil
}

// buildRoute builds one route's transcriber with the same profile adjustments
// the default route gets, so a routed window decodes under the same VAD and
// request limits as an unrouted one; only the model differs.
func (s *Service) buildRoute(prof provider.Profile, key string) (routedSTT, error) {
	prof.STTLanguage = key
	prof.STTVAD = s.cfg.MediaVAD
	prof.STTMaxPayloadMB = s.cfg.MediaSTTMaxPayloadMB
	prof.STTRequestTimeoutSec = s.cfg.MediaSTTRequestTimeoutSec
	tr, err := buildTranscriber(prof)
	if err != nil {
		return routedSTT{}, fmt.Errorf("build STT route %q for language %q: %w", prof.Name, key, err)
	}
	return routedSTT{stt: tr, route: strings.TrimSpace(prof.Name), coverage: append([]string(nil), prof.STTLanguages...)}, nil
}

// identifyWindow asks the §8.2.3 identifier for the window's language. It
// returns ok=false, which is "no signal", when no identifier is bound, the
// language is pinned, the request fails, or the report is empty or below the
// floor; a failure is logged and never fails the window.
func (s *Service) identifyWindow(ctx context.Context, relPath string, pieces []windowPiece, core CoverageRange, st *windowLanguageState) (string, float64, bool) {
	if s.identifier == nil || st.pinned || len(pieces) == 0 {
		return "", 0, false
	}
	data := s.identifierProbe(ctx, pieces, st)
	res, err := s.transcribeResult(ctx, s.identifier.stt, relPath, data)
	if err != nil {
		s.getLogger().Printf("windowed transcription: language identifier %q failed on window [%d,%d]ms of %s: %v; falling back to the decoder's report (SPEC §8.2.3)",
			s.identifier.route, core.StartMS, core.EndMS, relPath, err)
		return "", 0, false
	}
	tag := provider.PrimarySubtag(res.Language)
	if tag == "" {
		return "", 0, false
	}
	if res.LanguageConfidence > 0 && res.LanguageConfidence < windowLanguageMinConfidence {
		return "", res.LanguageConfidence, false
	}
	return tag, res.LanguageConfidence, true
}

// identifierProbe is the audio sent to the identifier: a centred slice of at
// most media.stt.language_probe_sec of the window when the window is longer
// and a cutter is available, else the window's first piece. The choice depends
// only on the window bounds, so it is deterministic for identical audio.
func (s *Service) identifierProbe(ctx context.Context, pieces []windowPiece, st *windowLanguageState) []byte {
	first := pieces[0]
	probeMS := s.cfg.MediaSTTLanguageProbeSec * 1000
	start, end := first.startMS, pieces[len(pieces)-1].endMS
	if st.cut == nil || probeMS <= 0 || end-start <= probeMS {
		return first.data
	}
	mid := start + (end-start)/2
	if data, err := st.cut(ctx, mid-probeMS/2, mid+probeMS/2); err == nil && len(data) > 0 {
		return data
	}
	return first.data
}

// windowVerdict is the §8.2.2 judgement of one decoded window on one route:
// refused for its language, refused by the quality gate, or kept (covered or
// not).
type windowVerdict struct {
	reason  string // "" when kept
	finding *quality.Finding
	covered bool
}

// judgeWindow applies the per-window floor and the per-window quality gate to
// one route's decode of a window.
func (s *Service) judgeWindow(results []pieceResult, cur routedSTT, lang string, core CoverageRange) windowVerdict {
	covered := routeCovers(cur, lang)
	if !covered && s.onUncoveredLanguage == onUncoveredLanguageSkip {
		return windowVerdict{reason: model.RefusedLanguageUncovered, covered: false}
	}
	if finding := s.windowQualityFinding(results, lang, core); finding != nil {
		return windowVerdict{reason: model.RefusedQualityGate, finding: finding, covered: covered}
	}
	return windowVerdict{covered: covered}
}

// fallThroughCandidates is the §8.2.3 candidate rule: while the window is
// refused and a later eligible candidate exists for its language, that
// candidate decodes the window and is judged in turn. A candidate that fails to
// decode ends the walk with a failed window (failErr), not a refusal.
func (s *Service) fallThroughCandidates(ctx context.Context, relPath string, pieces []windowPiece, plan windowSchedule, core CoverageRange, lang string, results []pieceResult, cur routedSTT, v windowVerdict) ([]pieceResult, routedSTT, windowVerdict, error) {
	if v.reason == "" {
		return results, cur, v, nil
	}
	cands, err := s.routeCandidates(lang)
	if err != nil || len(cands) < 2 {
		return results, cur, v, nil
	}
	next := 0
	for i, c := range cands {
		if c.route == cur.route {
			next = i + 1
			break
		}
	}
	for ; next < len(cands) && v.reason != ""; next++ {
		cand := cands[next]
		s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s refused on %q (%s); trying candidate %q (SPEC §8.2.3)",
			plan.label, core.StartMS, core.EndMS, relPath, cur.route, v.reason, cand.route)
		r, derr := s.decodePieces(ctx, relPath, cand.stt, pieces, plan)
		if len(r) == 0 {
			if derr == nil {
				derr = fmt.Errorf("candidate %q returned nothing", cand.route)
			}
			return nil, cand, windowVerdict{}, derr
		}
		results, cur = r, cand
		v = s.judgeWindow(results, cur, lang, core)
	}
	return results, cur, v, nil
}

// resolveWith is resolve with the §8.2.3 identifier ahead of the decoder's
// report: a pin still wins, then the identifier, then resolve's own order.
func (st *windowLanguageState) resolveWith(results []pieceResult, idTag string, idConf float64, idOK bool) (lang, source string, conf *float64) {
	if st.pinned || !idOK {
		return st.resolve(results)
	}
	if idConf > 0 {
		c := idConf
		return idTag, langSourceDetected, &c
	}
	return idTag, langSourceDetected, nil
}
