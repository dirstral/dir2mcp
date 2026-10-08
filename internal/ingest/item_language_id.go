package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

// This file implements SPEC §8.2.4 (dir2mcp #1059, #1060) under the default
// media.stt.language_scope: item. With media.stt.language_identifier bound, the
// item's language is resolved BEFORE transcription from a probe of the audio,
// and the first eligible media.stt.language_providers candidate for that
// language decodes the item; no match, or no signal, leaves the default profile
// decoding exactly as before. Under media.stt.on_route_error: default a route
// that fails is replaced once by the default profile. The decision is recorded
// on the transcript (route, route_fallback_from) and cached beside the text, so
// a cache hit restores the same record.

// itemRoute is the §8.2.4 record of how one item-scoped recording was routed:
// the language the identifier resolved, its confidence (0 when the identifier
// reported none), the profile that decoded, the candidate that failed before
// the default decoded (fallback), and whether the decoding profile declares the
// language (the §8.2.1 floor). It is persisted as the transcript cache entry's
// .route.json sidecar.
type itemRoute struct {
	Language     string  `json:"language"`
	Confidence   float64 `json:"confidence,omitempty"`
	Route        string  `json:"route"`
	FallbackFrom string  `json:"fallback_from,omitempty"`
	Uncovered    bool    `json:"uncovered,omitempty"`
}

// itemIdentifierActive reports whether the §8.2.4 item-scope identifier runs:
// item scope, an identifier bound, and no operator pin (a pin disables
// detection for every item, exactly as it does per window).
func (s *Service) itemIdentifierActive() bool {
	return !s.windowScoped() && s.identifier != nil && provider.PrimarySubtag(s.transcriptLanguage) == ""
}

// transcribeItemRouted is the §8.2.4 item decode: stage the media once, ask the
// identifier for the item's language on a probe, decode on the route that
// language selects (the default profile when none does), and under
// on_route_error=default decode once more on the default profile when the
// route fails. The returned record is nil when the identifier gave no signal,
// in which case the item was decoded exactly as without an identifier.
func (s *Service) transcribeItemRouted(ctx context.Context, relPath string, content []byte) (string, []model.TimedWord, *TranscriptCoverage, *itemRoute, error) {
	def := s.defaultRoute()
	if len(content) == 0 {
		text, words, err := s.transcribeWith(ctx, def.stt, relPath, content)
		return text, words, nil, nil, err
	}
	tmpPath, cleanup, err := stageMediaTemp(content, filepath.Ext(relPath))
	if err != nil {
		s.getLogger().Printf("item language identification %s: stage failed (%v); decoding on the default profile", relPath, err)
		text, words, cov, derr := s.transcribeStructuredWindowed(ctx, relPath, content, def.stt)
		return text, words, cov, nil, derr
	}
	defer cleanup()
	totalMS := s.probeStagedDurationMS(ctx, tmpPath)

	tag, conf, ok := s.identifyItem(ctx, relPath, content, tmpPath, totalMS)
	if !ok {
		text, words, cov, derr := s.decodeStagedTranscript(ctx, relPath, content, tmpPath, totalMS, def.stt)
		return text, words, cov, nil, derr
	}
	route, rerr := s.routeForLanguage(tag)
	if rerr != nil {
		s.getLogger().Printf("item language identification %s: language %q stays on the default profile %q: %v", relPath, tag, def.route, rerr)
		route = def
	}
	rec := &itemRoute{Language: tag, Confidence: conf, Route: route.route, Uncovered: !routeCovers(route, tag)}
	s.getLogger().Printf("item language identification %s: identifier %q resolved %q; route %q decodes (SPEC §8.2.4)", relPath, s.identifier.route, tag, route.route)
	text, words, cov, derr := s.decodeStagedTranscript(ctx, relPath, content, tmpPath, totalMS, route.stt)
	if derr != nil && s.routeErrorFallback && route.route != def.route {
		// SPEC §8.2.4: one more decode, on the default profile, and the record
		// names the candidate that failed. The default's own failure is the
		// item's failure.
		s.getLogger().Printf("item language identification %s: route %q failed: %v; decoding once on the default profile %q (media.stt.on_route_error=default, SPEC §8.2.4)", relPath, route.route, derr, def.route)
		rec.FallbackFrom, rec.Route, rec.Uncovered = route.route, def.route, !routeCovers(def, tag)
		text, words, cov, derr = s.decodeStagedTranscript(ctx, relPath, content, tmpPath, totalMS, def.stt)
	}
	return text, words, cov, rec, derr
}

// identifyItem asks the §8.2.3 identifier for the item's language on a probe:
// a centred slice of at most media.stt.language_probe_sec when the recording is
// longer and can be cut, else the whole audio. The choice depends only on the
// duration, so it is deterministic for identical audio. ok is false, "no
// signal", when the request fails, the report is empty, or it is below the
// floor; a failure is logged and never fails the item (SPEC §8.2.4).
func (s *Service) identifyItem(ctx context.Context, relPath string, content []byte, tmpPath string, totalMS int) (string, float64, bool) {
	data := content
	probeMS := s.cfg.MediaSTTLanguageProbeSec * 1000
	if probeMS > 0 && totalMS > probeMS {
		mid := totalMS / 2
		if cut, err := s.extractMediaSegment(ctx, tmpPath, mid-probeMS/2, mid+probeMS/2); err == nil && len(cut) > 0 {
			data = cut
		}
	}
	res, err := s.transcribeResult(ctx, s.identifier.stt, relPath, data)
	if err != nil {
		s.getLogger().Printf("item language identification %s: identifier %q failed: %v; decoding on the default profile (SPEC §8.2.4)", relPath, s.identifier.route, err)
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

// applyItemRouteMeta stamps the §8.2.4 record onto an item-scoped transcript's
// meta. Whenever the configuration binds an identifier under item scope the
// meta records the identifier, the scope and the route table, because those
// join the derivation identity (activeTranscriptIdentity keys on the
// configured binding, exactly as the window path does), so a pinned corpus or
// an identifier that could not be built still records what the identity
// expects and is not re-derived on every run. When the identifier resolved a
// language for this item, that language replaces the text detector's as the
// representation language (the identifier outranks text detection, §8.2.3),
// and route / route_fallback_from say which profile decoded. A nil record is
// "no signal": the item was decoded by the default profile and keeps the text
// detector's language.
func (s *Service) applyItemRouteMeta(meta *transcriptMeta, rec *itemRoute) {
	if s.windowScoped() {
		return
	}
	routes := itemLanguageRoutesRecord(s.languageRouteIDs)
	if routes == "" {
		return
	}
	meta.LanguageScope = languageScopeItem
	meta.LanguageRoutes = routes
	meta.LanguageIdentifier = strings.TrimSpace(s.cfg.MediaSTTLanguageIdentifier)
	if rec == nil {
		return
	}
	meta.Language = rec.Language
	meta.LanguageSource = langSourceDetected
	meta.LanguageConfidence = nil
	if rec.Confidence > 0 {
		c := rec.Confidence
		meta.LanguageConfidence = &c
	}
	meta.Route = rec.Route
	meta.RouteFallbackFrom = rec.FallbackFrom
	// The §8.2.1 covered fact is evaluated against the profile that decoded,
	// not the default: it is set only to false, absence being "no assertion".
	meta.LanguageCovered = nil
	if rec.Uncovered {
		no := false
		meta.LanguageCovered = &no
	}
}

// readCachedItemRoute loads the §8.2.4 route record cached beside a transcript,
// nil when there is none or it is unreadable (the item then reads as "no
// signal", the conservative outcome).
func readCachedItemRoute(path string) *itemRoute {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec itemRoute
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Route == "" {
		return nil
	}
	return &rec
}

// publishCachedItemRoute makes the cache entry's route sidecar match this
// decode and reports whether the text may be published alongside it, exactly as
// publishCachedCoverage does for the coverage sidecar: a record that cannot be
// written withholds the text cache, so the next run decodes again rather than
// restoring a transcript whose route it cannot name. A nil record removes a
// stale sidecar from an earlier decode of the same bytes.
func (s *Service) publishCachedItemRoute(path string, rec *itemRoute) bool {
	if rec == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.getLogger().Printf("clear stale transcript route cache (%s): %v; not caching this transcript", path, err)
			return false
		}
		return true
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		s.getLogger().Printf("marshal transcript route cache: %v; not caching this transcript", err)
		return false
	}
	if err := statefs.WriteFile(path, raw); err != nil {
		s.getLogger().Printf("write transcript route cache (%s): %v; not caching this transcript", path, err)
		return false
	}
	return true
}
