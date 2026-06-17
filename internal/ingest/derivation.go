package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/provider"
)

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

// sttTranscriptMetaJSON builds the meta_json persisted on a bare
// machine-transcribed transcript representation. It records source=stt plus the
// active STT derivation identity (provider/model/language, §5.2/§8.6.7) so a
// later STT model swap can be detected and the transcript re-derived. Fields are
// omitted (omitempty) when unset, so a setup with no resolved STT identity still
// produces valid meta_json (and an empty recorded identity that the gate treats
// as "always passes").
func (s *Service) sttTranscriptMetaJSON() (string, error) {
	meta := transcriptMeta{
		Source:     sttSource,
		Language:   strings.TrimSpace(s.transcriptLanguage),
		Timestamps: true,
		Provider:   strings.TrimSpace(s.sttProvider),
		Model:      strings.TrimSpace(s.sttModel),
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// activeTranscriptIdentity is the STT derivation identity of the currently
// configured transcriber, in the same canonical form derivationIdentity emits
// for a recorded transcript representation. It is compared against the identity
// read from a stored transcript to decide whether an STT model swap has made the
// transcript stale (§8.6.7).
func (s *Service) activeTranscriptIdentity() string {
	return derivationIdentity(string(provider.CapSTT),
		s.sttProvider, s.sttModel, "", s.transcriptLanguage)
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
	if s.extractor == nil {
		return false
	}
	active := s.activeOCRIdentity()
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

// ocrCacheKey returns the on-disk OCR cache filename stem for the given content,
// folding the active OCR derivation identity into the key (SPEC §8.6.7). Keying
// on the bytes alone would return the previous extractor's cached text after an
// OCR model swap that the re-ingest gate forced re-extraction for, silently
// defeating the re-derivation. An empty active identity (no extractor / no
// model concept) folds in "", preserving the historical bytes-only key.
func (s *Service) ocrCacheKey(content []byte) string {
	active := s.activeOCRIdentity()
	if active == "" {
		return computeContentHash(content)
	}
	combined := strings.Join([]string{computeContentHash(content), active}, "\x00")
	return computeContentHash([]byte(combined))
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
	return derivationIdentity(string(provider.CapSTT),
		meta.Provider, meta.Model, meta.ModelVersion, meta.Language), true
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
