package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/langdetect"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// detectLanguage runs best-effort representation language auto-detection on text
// when enabled (SPEC §8.8), returning the BCP-47 primary subtag and confidence,
// or ("", 0, false) when detection is disabled or the result is unknown/below
// the confidence floor. Detection never fails the caller; unknown is a
// first-class, non-error state.
func (s *Service) detectLanguage(text string) (tag string, confidence float64, ok bool) {
	if s == nil || !s.cfg.LanguageDetectionEnabled {
		return "", 0, false
	}
	return langdetect.Detect(text, langdetect.DefaultMinConfidence)
}

// Representation provenance and identity-driven re-derivation (spec §8.6.7).
//
// Derived representations (transcripts, extracted markdown) record the
// provider+model that produced them in meta_json (§5.2). The re-ingest gate
// builds a "derivation identity" string from those structured fields and
// compares it to the active model's identity; on mismatch the representation is
// stale and is re-derived → re-chunked → re-embedded. This is the
// per-representation analogue of the corpus-lifetime embed identity (§8.1.4):
// the comparison is built from explicit fields (capability/provider/model/
// version/language), never an opaque blob, and an EMPTY recorded identity always
// passes so a pre-upgrade corpus (reps written before provenance was persisted)
// is never mass re-derived on first upgrade.

// Diarizer is the injectable model-driven speaker-diarization seam (SPEC §8.6.8).
// Given the source media bytes and the transcript's time-spanned segments, it
// returns a speaker id (and optional label) per segment ordinal. dir2mcp ships no
// default implementation: the diarization-capable backend (self-hosted
// WhisperX/pyannote) is integrated out-of-band and injected via SetDiarizer. The
// returned ids MUST be stable and deterministic across re-runs of the same media
// with the same backend (§8.6.8); an implementation that cannot attribute a
// segment returns an empty id for it (that segment degrades to un-attributed).
type Diarizer interface {
	Diarize(ctx context.Context, media []byte, segments []SpeakerSegment) ([]SpeakerAttribution, error)
}

// SpeakerSegment is one transcript segment handed to a Diarizer: its [start,end]
// in ms and its text. It is a stable, dependency-light shape so the seam does not
// expose internal chunk types.
type SpeakerSegment struct {
	StartMS int
	EndMS   int
	Text    string
}

// SpeakerAttribution is a Diarizer's result for one segment (by position): the
// stable speaker id and an optional human-readable label. An empty ID means the
// segment is left un-attributed.
type SpeakerAttribution struct {
	ID    string
	Label string
}

// activeDiarizeIdentity is the diarization derivation identity of the currently
// configured diarizer (SPEC §8.6.7/§8.6.8), in the canonical derivationIdentity
// form. Empty when diarization is inactive, so it folds nothing into the
// transcript identity and never forces a re-derivation on a non-diarized corpus.
func (s *Service) activeDiarizeIdentity() string {
	if !s.diarizeActive {
		return ""
	}
	return derivationIdentity(string(provider.CapDiarize),
		s.diarizeProvider, s.diarizeModel, "", "")
}

// sttTranscriptMetaJSON builds the meta_json persisted on a bare
// machine-transcribed transcript representation. It records source=stt plus the
// active STT derivation identity (provider/model/language, §5.2/§8.6.7) so a
// later STT model swap can be detected and the transcript re-derived. When
// diarization is active it additionally records diarized + the diarize
// provider/model (§8.6.8) so a diarize-backend swap re-derives too. hasWords
// sets the §8.6.9 `words` granularity flag when the transcript's segments carry
// per-word timing. Fields are
// omitted (omitempty) when unset, so a setup with no resolved STT identity still
// produces valid meta_json (and an empty recorded identity that the gate treats
// as "always passes").
func (s *Service) sttTranscriptMetaJSON(speakers []Speaker, text string, hasWords bool) (string, error) {
	meta := transcriptMeta{
		Source:     sttSource,
		Language:   strings.TrimSpace(s.transcriptLanguage),
		Timestamps: true,
		// §8.6.9: declare word-level timing granularity when at least one segment
		// carries a populated words array (omitempty ⇒ segment-only stays absent).
		Words:    hasWords,
		Provider: strings.TrimSpace(s.sttProvider),
		Model:    strings.TrimSpace(s.sttModel),
	}
	// §8.8 precedence: an operator pin (media.language / per-provider
	// stt_language, §16.2) is the "configured" language and always wins. With no
	// pin, fall back to best-effort auto-detection on the transcript text
	// ("detected"). A detected language is recorded as the effective language but
	// is deliberately excluded from the derivation identity (see
	// transcriptIdentityFromMeta): a pure detector change must not re-transcribe.
	if meta.Language != "" {
		meta.LanguageSource = langSourceConfigured
	} else if tag, conf, ok := s.detectLanguage(text); ok {
		meta.Language = tag
		meta.LanguageSource = langSourceDetected
		meta.LanguageConfidence = &conf
	}
	if s.diarizeActive {
		// Record the diarize backend identity whenever diarization is active so a
		// backend/model swap re-derives the transcript (§8.6.7), even before any
		// attribution lands. `diarized` and the speakers set reflect the
		// attribution actually present on the segments (§5.2): a capable backend
		// with no diarizer wired yet yields no speakers, so diarized stays false.
		meta.DiarizeProvider = strings.TrimSpace(s.diarizeProvider)
		meta.DiarizeModel = strings.TrimSpace(s.diarizeModel)
		if len(speakers) > 0 {
			meta.Diarized = true
			meta.Speakers = speakers
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// activeTranscriptIdentity is the STT (+ diarize, when active) derivation
// identity of the currently configured transcriber, in the same canonical form
// derivationIdentity emits for a recorded transcript representation. It is
// compared against the identity read from a stored transcript to decide whether
// an STT — or diarization (§8.6.8) — model swap has made the transcript stale
// (§8.6.7). The diarize identity is appended only when diarization is active, so
// a non-diarized corpus's identity is unchanged.
func (s *Service) activeTranscriptIdentity() string {
	return joinTranscriptIdentity(
		derivationIdentity(string(provider.CapSTT), s.sttProvider, s.sttModel, "", s.transcriptLanguage),
		s.activeDiarizeIdentity())
}

// joinTranscriptIdentity appends a non-empty diarize identity to the STT
// identity with a stable separator, so a diarize-backend change re-derives the
// transcript (§8.6.7/§8.6.8). An empty diarize identity returns the STT identity
// unchanged, keeping a non-diarized transcript's recorded identity byte-stable.
func joinTranscriptIdentity(sttIdentity, diarizeIdentity string) string {
	if strings.TrimSpace(diarizeIdentity) == "" {
		return sttIdentity
	}
	return sttIdentity + "#" + diarizeIdentity
}

// ActiveOCRIdentity is the exported accessor for the OCR/extraction derivation
// identity ingest folds into the OCR cache key it writes (ocrCacheKey). It is
// byte-identical to that fold: empty when no extractor is configured (the
// bytes-only key path), otherwise the canonical CapOCR identity. The retriever
// plumbs this into open_file via SetDerivationCacheIdentities so the OCR cache
// LOOKUP keys the cache the SAME identity-aware way ingest's writer does (issue
// #488). It is a thin wrapper: the identity computation stays in activeOCRIdentity.
func (s *Service) ActiveOCRIdentity() string {
	return s.activeOCRIdentity()
}

// ActiveTranscriptIdentity is the exported accessor for the transcript
// derivation identity ingest folds into the transcript cache key it writes
// (transcriptCacheKey). It returns EXACTLY what that key folds, which is the
// crucial detail for byte-identity: transcriptCacheKey uses the bytes-only key
// (folds nothing) when neither an STT provider nor model is resolved, even though
// the underlying activeTranscriptIdentity always renders a non-empty "stt|…"
// string. This accessor therefore returns "" on that same no-STT guard so the
// retriever's open_file lookup lands on ingest's bytes-only key instead of
// folding a spurious identity and missing (issue #488). With STT configured it
// returns the full canonical CapSTT(+diarize) identity, identical to what
// transcriptCacheKey folds.
func (s *Service) ActiveTranscriptIdentity() string {
	// Mirror transcriptCacheKey's guard exactly (internal/ingest/derivation.go):
	// no resolved STT provider+model ⇒ ingest writes the bytes-only key, so the
	// retriever must fold no identity.
	if strings.TrimSpace(s.sttProvider) == "" && strings.TrimSpace(s.sttModel) == "" {
		return ""
	}
	return s.activeTranscriptIdentity()
}

// ActiveDerivationIdentities computes the ACTIVE OCR and transcript derivation
// identities (SPEC §8.6.7) for cfg WITHOUT constructing a full ingest Service or
// touching a store. It exists for retriever-only paths that have no ingest
// Service to borrow the getters from — the read-only `ask` CLI — so open_file's
// OCR/transcript cache lookup can still be keyed the identity-aware way ingest's
// writer keys it (issue #488). The returned strings are byte-identical to
// (*Service).ActiveOCRIdentity / ActiveTranscriptIdentity on a Service built from
// the same cfg the way the CLI builds its ingestor (NewService + the config
// extractor), because both resolve the transcript fields through the shared
// resolveTranscriptIdentityFields and the extractor through DocumentExtractorFromConfig.
// Prefer a live Service's getters when one exists on the path; use this only
// where none does.
func ActiveDerivationIdentities(cfg config.Config) (ocr, transcript string) {
	s := &Service{cfg: cfg}
	// The extractor is not resolved by NewService; the CLI ingestor wires it via
	// SetDocumentExtractor(DocumentExtractorFromConfig(cfg)). Mirror that here so
	// the OCR identity matches what ingest actually runs with.
	if extractor := DocumentExtractorFromConfig(cfg); extractor != nil {
		s.extractor = extractor
	}
	s.resolveTranscriptIdentityFields()
	return s.ActiveOCRIdentity(), s.ActiveTranscriptIdentity()
}

// activeOCRIdentity is the OCR/extraction derivation identity of the currently
// configured extractor, in the same canonical form derivationIdentity emits for
// the stored extracted_markdown representation's meta_json. Empty when no
// extractor is configured.
func (s *Service) activeOCRIdentity() string {
	prov, modelName := s.extractorProviderModel()
	if prov == "" && modelName == "" {
		return ""
	}
	return derivationIdentity(string(provider.CapOCR), prov, modelName, "", "")
}

// activeOCRIdentityForPath is activeOCRIdentity resolved for a SPECIFIC document's
// format. Under `ingest.extractor: auto` two extraction engines can be active at
// once (docling + the capability-activated pandoc, #393), and a born-digital
// document extracted by pandoc records provider "pandoc" in its meta_json — not
// the primary engine's provider. So the staleness comparison must be against the
// identity of the engine that WOULD extract THIS format (its per-format route),
// not the global primary; otherwise every pandoc-extracted doc looks perpetually
// stale under auto (recorded "pandoc" vs a primary "docling") and re-extracts on
// every run. A format that routes to no extraction engine (degrade/raw_text) has
// no active OCR identity, so the check self-skips (fail-open).
func (s *Service) activeOCRIdentityForPath(relPath string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	switch s.routeExtractionExt(ext) {
	case routePandoc:
		// pandoc has no model concept; mirror pandocExtractionMetaJSON (provider
		// "pandoc", no model/version) so recorded == active for an unchanged doc.
		return derivationIdentity(string(provider.CapOCR), "pandoc", "", "", "")
	case routeStructured, routeFlatOCR:
		// The primary extractor is the structured/flat engine for these routes, so
		// its identity is the right one.
		return s.activeOCRIdentity()
	default:
		return ""
	}
}

// derivationIdentityStale reports whether a document whose content is unchanged
// must nonetheless be reprocessed because a derived representation's recorded
// derivation identity no longer matches the active model's identity (spec
// §8.6.7). It probes the bare transcript representation against the active STT
// identity and the extracted_markdown representation against the active OCR
// identity. A mismatch on either returns true and logs a scoped warn (mirroring
// the embed-identity → reindex UX, §8.1.4) so re-derivation is observable.
//
// It NEVER invalidates on:
//   - an empty recorded identity (pre-upgrade reps, backward-compat): the first
//     upgrade must not force a corpus-wide re-derivation;
//   - a sidecar-sourced transcript: authored, not model-derived (§8.6.4);
//   - a missing store capability, a missing document, or a representation the
//     active pipeline cannot produce (e.g. an OCR rep with STT off): fail open.
//
// The caller is responsible for only invoking this on the content-unchanged,
// status==ok path; under --force/reindex the normal gate already returns true.
func (s *Service) derivationIdentityStale(ctx context.Context, relPath string) bool {
	reader, ok := s.store.(representationMetaReader)
	if !ok {
		return false
	}
	if s.transcriptStale(ctx, reader, relPath) {
		return true
	}
	return s.ocrStale(ctx, reader, relPath)
}

// transcriptStale reports whether the document's stored machine transcript was
// produced by a different STT model than the active one (§8.6.7). It self-skips
// (returns false) when STT is off (no active identity to compare against), so a
// run that simply has no transcriber configured never invalidates an existing
// transcript.
func (s *Service) transcriptStale(ctx context.Context, reader representationMetaReader, relPath string) bool {
	if s.transcriber == nil {
		return false
	}
	active := s.activeTranscriptIdentity()
	metaJSON, err := reader.RepresentationMetaByType(ctx, relPath, RepTypeTranscript)
	if err != nil || metaJSON == "" {
		return false
	}
	recorded, ok := transcriptIdentityFromMeta(metaJSON)
	if !ok || recorded == active {
		return false
	}
	s.getLogger().Printf(
		"STT model changed for %s (transcript derived with %q, configured %q); re-transcribing this representation",
		relPath, recorded, active)
	return true
}

// ocrStale reports whether the document's stored extracted_markdown was produced
// by a different OCR/extraction model than the active one (§8.6.7). It self-skips
// when no extractor is configured.
func (s *Service) ocrStale(ctx context.Context, reader representationMetaReader, relPath string) bool {
	if s.extractor == nil && s.pandocExtractor == nil {
		return false
	}
	// Route-aware: under `auto`, a pandoc-extracted born-digital doc (#393) must be
	// compared against the pandoc identity, not the primary engine's.
	active := s.activeOCRIdentityForPath(relPath)
	if active == "" {
		return false
	}
	metaJSON, err := reader.RepresentationMetaByType(ctx, relPath, RepTypeExtractedMarkdown)
	if err != nil || metaJSON == "" {
		return false
	}
	recorded, ok := ocrIdentityFromMeta(metaJSON)
	if !ok || recorded == active {
		return false
	}
	s.getLogger().Printf(
		"OCR model changed for %s (extraction derived with %q, configured %q); re-extracting this representation",
		relPath, recorded, active)
	return true
}

// transcriptCacheKey returns the on-disk transcript cache filename stem for the
// given media bytes, folding the active STT derivation identity into the key
// (SPEC §8.6.7) so a model swap does not return the previous model's cached
// transcript. When no STT provider/model is resolved (no transcriber, or an
// identity with empty provider+model) the historical bytes-only key is preserved.
func (s *Service) transcriptCacheKey(content []byte) string {
	if strings.TrimSpace(s.sttProvider) == "" && strings.TrimSpace(s.sttModel) == "" {
		return computeContentHash(content)
	}
	combined := strings.Join([]string{computeContentHash(content), s.activeTranscriptIdentity()}, "\x00")
	return computeContentHash([]byte(combined))
}

// ocrCacheKey returns the on-disk OCR cache filename stem for the given content,
// folding the active OCR derivation identity into the key (SPEC §8.6.7). Keying
// on the bytes alone would return the PREVIOUS extractor's cached text after an
// OCR model/provider swap that the re-ingest gate forced re-extraction for,
// silently defeating the re-derivation.
//
// The identity is folded whenever it is non-empty — which includes PROVIDER-only
// identities (docling / docling-serve / custom commands, which have no model).
// All extractors share one `cache/ocr` directory, so a docling↔docling-serve
// swap (which ocrStale treats as stale, since the provider differs) MUST land on
// a different cache key; folding the provider-bearing identity guarantees that.
// An empty active identity (no extractor configured) folds in nothing, preserving
// the historical bytes-only key for the no-OCR path.
func (s *Service) ocrCacheKey(content []byte) string {
	identity := s.activeOCRIdentity()
	if identity == "" {
		return computeContentHash(content)
	}
	combined := strings.Join([]string{computeContentHash(content), identity}, "\x00")
	return computeContentHash([]byte(combined))
}

// OCRCacheKey exposes ocrCacheKey for tests in the tests/ tree, so the
// provider/model → cache-key binding (SPEC §8.6.7) can be asserted directly —
// in particular that a provider-only swap (docling↔docling-serve) yields a
// distinct key even though neither extractor carries a model.
func (s *Service) OCRCacheKey(content []byte) string {
	return s.ocrCacheKey(content)
}

// activeTranslateIdentity is the translation derivation identity of the
// currently configured translator for a given target language, in the canonical
// derivationIdentity form (capability=translate, §8.6.2/§8.6.7). The target
// language is folded as the identity's language field so a different target lands
// on a different identity (and cache key). Empty when no translate provider/model
// is resolved, so the historical bytes+text-only key is preserved for the
// no-translate path.
func (s *Service) activeTranslateIdentity(targetLang string) string {
	if strings.TrimSpace(s.translateProvider) == "" && strings.TrimSpace(s.translateModel) == "" {
		return ""
	}
	// Fold the chat translate window/margin shape into the identity's version
	// field (issue #573): the windowed 1:1 batch prompt gives the model cross-cue
	// context, so its output differs from the isolated per-line prompt and from a
	// different window shape. A stale cached translation would be mislabeled by the
	// rep's recorded provider/model, so a shape change MUST miss the cache. The
	// historical per-line shape folds "" so pre-windowing caches stay valid.
	return derivationIdentity(string(provider.CapTranslate),
		s.translateProvider, s.translateModel, s.translateWindowShape(), targetLang)
}

// translateCacheKey returns the on-disk translation cache filename stem for the
// given source media bytes, source transcript text, and target language. It folds
// the canonical translate derivation identity (provider/model/target-language)
// into the key (SPEC §8.6.7), exactly mirroring transcriptCacheKey/ocrCacheKey,
// so the same source in two corpora derives the translation once (cross-corpus
// reuse) while a provider, model, or target-language change MISSES the cache and
// never returns another derivation's bytes.
//
// The SOURCE TRANSCRIPT TEXT is folded in addition to the media bytes because the
// translation is of that text: if an upstream STT/model swap changes the source
// transcript, the cached translation of the OLD text is stale and must miss. When
// no translate provider/model is resolved the historical bytes+text-only key is
// preserved.
func (s *Service) translateCacheKey(content []byte, sourceText, targetLang string) string {
	parts := []string{
		computeContentHash(content),
		computeContentHash([]byte(sourceText)),
	}
	if identity := s.activeTranslateIdentity(targetLang); identity != "" {
		parts = append(parts, identity)
	} else {
		// Preserve the historical behaviour of keying on the lower-cased target
		// language even with no resolved provider/model identity, so two distinct
		// targets never collide on the no-identity path.
		parts = append(parts, strings.ToLower(strings.TrimSpace(targetLang)))
	}
	combined := strings.Join(parts, "\x00")
	return computeContentHash([]byte(combined))
}

// TranslateCacheKey exposes translateCacheKey for tests in the tests/ tree, so
// the provider/model/target-language → cache-key binding (SPEC §8.6.7) can be
// asserted directly — in particular that a provider, model, or target-language
// change yields a distinct key (no cross-identity bleed) while the same identity
// over the same source yields a stable key (cross-corpus reuse).
func (s *Service) TranslateCacheKey(content []byte, sourceText, targetLang string) string {
	return s.translateCacheKey(content, sourceText, targetLang)
}

// derivationIdentity builds the canonical, order-stable derivation-identity
// string from the structured fields the spec defines (§8.6.7:
// {capability, provider, model, version, language}). It is intentionally NOT an
// opaque hash so the value is greppable in logs and diffable in tests. All
// fields are trimmed and lower-cased for the capability (a fixed token) while
// provider/model/version/language are trimmed only, preserving case that a
// provider may treat as significant in a model id.
func derivationIdentity(capability, prov, modelName, version, language string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		strings.ToLower(strings.TrimSpace(capability)),
		strings.TrimSpace(prov),
		strings.TrimSpace(modelName),
		strings.TrimSpace(version),
		strings.TrimSpace(language))
}

// transcriptIdentityFromMeta builds the recorded STT derivation identity of a
// stored transcript representation from its meta_json. It returns ("", false)
// for a transcript that carries no STT identity and MUST NOT be invalidated by
// an STT model change: a sidecar-sourced transcript (§8.6.4/§8.6.7), a
// translated transcript (its provenance is the translate provider, screened
// elsewhere), or a pre-upgrade transcript whose provider/model were never
// recorded (backward-compat: empty identity always passes). A non-empty result
// is in the same canonical form as activeTranscriptIdentity.
func transcriptIdentityFromMeta(metaJSON string) (string, bool) {
	metaJSON = strings.TrimSpace(metaJSON)
	if metaJSON == "" {
		return "", false
	}
	var meta transcriptMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		// Unparseable meta is treated as "no recorded identity": fail open rather
		// than force-re-transcribe on a meta we cannot read.
		return "", false
	}
	// Sidecar transcripts are authored, not model-derived: never invalidated by
	// an STT model change (§8.6.7).
	if strings.TrimSpace(meta.Source) == sidecarSource {
		return "", false
	}
	// A translated transcript's STT provenance is not the source-transcript STT
	// model; it is handled by the source transcript's re-derivation cascade.
	if strings.TrimSpace(meta.Source) == translationSource {
		return "", false
	}
	// Backward-compat: a pre-upgrade STT transcript recorded no provider/model.
	// An empty identity always passes so the first upgrade does not force a
	// corpus-wide re-transcription (mirrors VerifyEmbedIdentity's fresh-index
	// rule).
	if strings.TrimSpace(meta.Provider) == "" && strings.TrimSpace(meta.Model) == "" {
		return "", false
	}
	// A detected language (§8.8) is best-effort metadata, NOT part of the
	// derivation identity: a detector change must not force re-transcription. The
	// active identity (activeTranscriptIdentity) is built from the configured pin
	// only, so exclude a detected language here to keep the two comparable. A
	// configured/declared language remains part of the identity.
	identityLang := meta.Language
	if strings.TrimSpace(meta.LanguageSource) == langSourceDetected {
		identityLang = ""
	}
	sttIdentity := derivationIdentity(string(provider.CapSTT),
		meta.Provider, meta.Model, meta.ModelVersion, identityLang)
	// Fold the recorded diarize identity (§8.6.8) into the transcript identity so
	// a diarize provider/model change is detected as stale. Only model-derived
	// diarization records a provider/model; a sidecar-supplied <v> attribution
	// records diarized:true with NO provider/model, so it folds in nothing and is
	// never invalidated by a diarize-backend change (mirrors §8.6.7 for authored
	// sources). A pre-upgrade transcript with neither field is unchanged.
	diarizeIdentity := ""
	if strings.TrimSpace(meta.DiarizeProvider) != "" || strings.TrimSpace(meta.DiarizeModel) != "" {
		diarizeIdentity = derivationIdentity(string(provider.CapDiarize),
			meta.DiarizeProvider, meta.DiarizeModel, "", "")
	}
	return joinTranscriptIdentity(sttIdentity, diarizeIdentity), true
}

// ocrIdentityFromMeta builds the recorded OCR/extraction derivation identity of
// a stored extracted_markdown representation from its meta_json. It returns
// ("", false) when no provider/model was recorded (a pre-upgrade extraction, or
// an extractor whose meta carries no model): an empty identity always passes
// (backward-compat). A non-empty result is in the same canonical form as
// activeOCRIdentity.
func ocrIdentityFromMeta(metaJSON string) (string, bool) {
	metaJSON = strings.TrimSpace(metaJSON)
	if metaJSON == "" {
		return "", false
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return "", false
	}
	prov := strings.TrimSpace(meta["provider"])
	modelName := strings.TrimSpace(meta["model"])
	version := strings.TrimSpace(meta["model_version"])
	if prov == "" && modelName == "" {
		return "", false
	}
	return derivationIdentity(string(provider.CapOCR), prov, modelName, version, ""), true
}
