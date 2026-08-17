package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
	"golang.org/x/text/language"
)

// sidecarExtensions are the subtitle sidecar formats recognised next to a media
// file (spec §8.6.4). They are checked case-insensitively.
var sidecarExtensions = []string{".vtt", ".srt", ".ttml"}

// sidecarSource is the meta_json source value recorded on a sidecar-derived
// transcript representation. Sidecar transcripts are authored, not
// model-derived, so they carry no STT provider/model derivation identity and are
// NOT invalidated by an STT model change (spec §8.6.7).
const sidecarSource = "sidecar"

// sttSource is the meta_json source value recorded on a machine-transcribed
// transcript representation (spec §5.2 `source: stt`). Unlike a sidecar it is
// model-derived, so it carries an STT provider/model derivation identity and IS
// re-derived when the active STT model changes (§8.6.7).
const sttSource = "stt"

// translationSource is the meta_json source value recorded on a transcript
// representation produced by translating another transcript (spec §8.6.2). It is
// model-derived (unlike a sidecar), so it carries its translate provider/model
// derivation identity and IS screened by the output quality gate.
const translationSource = "translation"

// language_source values for transcriptMeta.LanguageSource (SPEC §5.2/§8.8).
// They record which signal won the §8.8 precedence (configured > declared >
// detected) for a representation's effective language.
//
// The third precedence rank, "detected" (a best-effort auto-detector's result,
// langSourceDetected), is emitted for plain-text (raw_text) representations via
// the langdetect detector (SPEC §8.8). It is recorded only when neither a pin
// (configured) nor a source declaration (declared) is present. Transcript and
// OCR detection is a follow-up: their meta language doubles as part of the
// derivation identity (§8.6.7), so recording a detected language there without
// decoupling it from that identity would force spurious re-derivation.
const (
	// langSourceConfigured: the language was pinned by an operator (e.g.
	// media.language / a per-provider stt_language). It always wins (§8.8).
	langSourceConfigured = "configured"
	// langSourceDeclared: the language was asserted by the source itself (a
	// sidecar's language suffix §8.6.4, a track/document language tag).
	langSourceDeclared = "declared"
	// langSourceDetected: a best-effort auto-detector determined the language
	// (§8.8). Lowest precedence — recorded only when no pin/declaration exists.
	langSourceDetected = "detected"
)

// sidecarFile is a discovered subtitle sidecar for a media document: its corpus
// rel_path, the language tag parsed from the filename (empty when the sidecar is
// undifferentiated, e.g. "clip.vtt"), the lowercased extension, and the sidecar
// file's modification time used for the mtime freshness gate (§7.6).
type sidecarFile struct {
	RelPath   string
	Lang      string
	Ext       string
	MTimeUnix int64
}

// transcriptMeta is the meta_json shape persisted on a transcript
// representation (spec §5.2). For sidecar transcripts Source is "sidecar" and
// the STT provider/model fields are omitted (omitempty) so the representation
// carries no model derivation identity (§8.6.7).
type transcriptMeta struct {
	Source   string `json:"source,omitempty"`
	Language string `json:"language,omitempty"`
	// LanguageSource records how Language was obtained (SPEC §5.2/§8.8):
	// "configured" (an operator pin, e.g. media.language / per-provider
	// stt_language), "declared" (asserted by the source itself, e.g. a sidecar's
	// language suffix or track tag), or "detected" (an auto-detector's best-effort
	// result). Omitted (omitempty) when no language was recorded (unknown) or the
	// provenance is unspecified, so meta_json is unchanged for a transcript that
	// records no language. The recorded Language is the EFFECTIVE value resolved
	// by §8.8 precedence (configured > declared > detected); LanguageSource names
	// the signal that won.
	LanguageSource string `json:"language_source,omitempty"`
	// LanguageConfidence is a detector-reported confidence in [0,1] for an
	// auto-"detected" Language (SPEC §5.2/§8.8). Informational only — it MUST NOT
	// be re-applied as a filter at query time (§9.5). Omitted unless a detector
	// produced it; a pointer so a genuine 0.0 is distinguishable from "absent".
	LanguageConfidence *float64 `json:"language_confidence,omitempty"`
	// LanguageCovered records honest STT language coverage (SPEC §8.2.1, #566).
	// It is set ONLY to false, and ONLY when the selected STT model DECLARES a
	// non-empty coverage set (a profile's stt_languages) and the effective
	// transcript Language falls outside it — the "transcribed in a language this
	// model does not declare" signal. It is a pointer with omitempty so it is
	// absent (coverage unknown / no declaration / covered) rather than a
	// misleading false: absence MUST be read as "no coverage assertion", never as
	// "covered". Sidecar transcripts (authored, not model-derived) never set it.
	LanguageCovered *bool  `json:"language_covered,omitempty"`
	Timestamps      bool   `json:"timestamps"`
	Format          string `json:"format,omitempty"`

	// Words records the transcript's finest CAPTURED timing granularity (SPEC
	// §8.6.9): true iff at least one segment carries a populated extra_json.words
	// array (per-word timing, §8.6.1). Omitted (omitempty) when no segment carries
	// word timing, so a segment-only transcript's meta_json is unchanged. Per
	// §8.6.9 a consumer MUST treat absent/false as "segment granularity only" and
	// degrade gracefully — never error because word timing is missing. It is a
	// discovery hint (a consumer can tell word timing is available without
	// inspecting every span); it never changes chunking or citations (§8.6.9).
	Words bool `json:"words,omitempty"`

	// Provider / Model / ModelVersion record the STT derivation identity on a
	// machine-transcribed transcript (Source == "stt", spec §5.2/§8.6.7): the
	// resolved STT provider profile name, its model, and an optional model
	// version. They are the runtime analogue of the embed identity (§8.1.4) and
	// drive per-representation re-derivation when the active STT model changes.
	// Omitted (omitempty) on sidecar transcripts (Source == "sidecar"), which are
	// authored — not model-derived — and MUST NOT be invalidated by an STT model
	// change (§8.6.7).
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`

	// SourceLanguage / TranslateProvider / TranslateModel are recorded only on a
	// translated transcript (Source == "translation", spec §8.6.2/§5.2): the
	// language it was translated from plus the chat provider/model that produced
	// it. Omitted (omitempty) on STT and sidecar transcripts.
	SourceLanguage    string `json:"source_language,omitempty"`
	TranslateProvider string `json:"translate_provider,omitempty"`
	TranslateModel    string `json:"translate_model,omitempty"`

	// Diarized / DiarizeProvider / DiarizeModel / Speakers record speaker
	// attribution on a diarized transcript (Source any; spec §8.6.8/§5.2).
	// Diarized is true when the transcript carries per-segment speakers. A
	// model-derived diarization additionally records DiarizeProvider/DiarizeModel
	// (part of the derivation identity, §8.6.7); a sidecar that supplied <v>
	// voice tags is diarized WITHOUT a model, so those stay empty (omitempty).
	// Speakers is the distinct set of {id,label} present, for discovery. All
	// fields are omitted on a non-diarized transcript so meta_json is unchanged.
	Diarized        bool      `json:"diarized,omitempty"`
	DiarizeProvider string    `json:"diarize_provider,omitempty"`
	DiarizeModel    string    `json:"diarize_model,omitempty"`
	Speakers        []Speaker `json:"speakers,omitempty"`

	// Track / TrackLanguage / TrackLabel record which audio stream a transcript was
	// produced from in a multi-track container (SPEC §8.6.12). Track is the 0-based
	// AUDIO-relative stream index and is recorded ONLY for an ADDITIONAL track
	// (N ≥ 1): track 0 omits it (absence ⇒ track 0), so a legacy single-track
	// transcript's meta_json is byte-for-byte unchanged. omitempty on a plain int is
	// safe here because an additional track index is always ≥ 1, so a genuine value
	// is never suppressed. TrackLanguage (BCP-47) and TrackLabel (e.g. "commentary")
	// carry the container's declared per-stream language tag / title when present,
	// and are absent otherwise.
	Track         int    `json:"track,omitempty"`
	TrackLanguage string `json:"track_language,omitempty"`
	TrackLabel    string `json:"track_label,omitempty"`
}

// Speaker is one distinct speaker recorded in a diarized transcript's meta_json
// (spec §8.6.8): the stable per-transcript id (e.g. "S1") and an optional
// human-readable label. The per-segment attribution lives on the segment "time"
// span's extra_json.speaker (§5.4); this set is for discovery/listing.
type Speaker struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// distinctSpeakers returns the distinct speakers present across the transcript
// segments in first-appearance order (deterministic, matching the S-id
// allocation order in chunkSubtitleCuesFiltered), for the transcript meta_json
// speakers set (SPEC §8.6.8). Segments with no speaker contribute nothing, so an
// undiarized transcript yields an empty slice (and meta omits the field).
func distinctSpeakers(segments []chunkSegment) []Speaker {
	seen := make(map[string]struct{})
	var out []Speaker
	for _, seg := range segments {
		id := strings.TrimSpace(seg.Span.Speaker)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, Speaker{ID: id, Label: strings.TrimSpace(seg.Span.SpeakerLabel)})
	}
	return out
}

// setSidecarIndex records the mtime of every discovered file by rel_path so the
// transcript path can detect media siblings (sidecars) and gate ingestion on
// their freshness without an extra filesystem stat. It is set once per scan from
// the same walk that drives discovery, so it works for any CorpusFS backend.
// PathExcludes are honoured here so an operator who excludes e.g. "**/*.vtt"
// never has those files read or persisted as transcripts (the exclude contract).
func (s *Service) setSidecarIndex(files []DiscoveredFile) {
	idx := make(map[string]int64, len(files))
	for _, f := range files {
		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			continue
		}
		idx[f.RelPath] = f.MTimeUnix
	}
	s.sidecarMu.Lock()
	s.sidecarIndex = idx
	s.sidecarMu.Unlock()
}

// sidecarsEnabled reports whether subtitle sidecar ingestion is active. It
// defaults to true (spec default media.sidecars.enabled: true) and is disabled
// only by an explicit opt-out.
func (s *Service) sidecarsEnabled() bool {
	return !s.cfg.MediaSidecarsDisabled
}

// findSidecars returns the subtitle sidecars sitting next to the media document,
// resolved through the corpus FS so it works for any backend. A sidecar matches
// when its path is "<base>.<ext>" (undifferentiated) or "<base>.<lang>.<ext>"
// (per-language), for either of two candidate bases, in this precedence order:
//
//  1. the EXACT base, the media path without its extension. This is the only
//     base that ever bound, so every existing binding is unchanged.
//  2. the NORMALIZED variant base (§8.6.5 normalized name), the media path with
//     its rendition markers stripped. It binds a sidecar that sits on the bare
//     stem of a rendition-suffixed media file ("<sha>.ru.vtt" next to
//     "<sha>_1080p.mp4"), which §8.6.4 requires to be ingested as the transcript
//     instead of running STT (issue #876).
//
// The two precedence rules in mergeSidecarCandidates keep pass 2 from
// overriding or duplicating pass 1. Results are sorted by (language, extension,
// rel_path) for deterministic selection. When the scan-built index is
// unavailable (e.g. a direct GenerateTranscriptRepresentation call in tests), it
// falls back to walking the corpus once.
func (s *Service) findSidecars(ctx context.Context, mediaRelPath string) []sidecarFile {
	index := s.sidecarIndexOrWalk(ctx)
	if len(index) == 0 {
		return nil
	}
	base := stripExt(mediaRelPath)
	// mediaExt is the media file's own extension token (e.g. "mp3"), lowercased
	// and without the leading dot. A sidecar whose single tail token equals it —
	// "clip.mp3.vtt" for media "clip.mp3" — is the media filename plus a subtitle
	// extension, not a language-tagged sidecar, so it must be rejected.
	mediaExt := strings.TrimPrefix(strings.ToLower(path.Ext(mediaRelPath)), ".")
	variantBase := variantSidecarBase(mediaRelPath, base)
	var exact, variant []sidecarFile
	for relPath, mtime := range index {
		ext := strings.ToLower(path.Ext(relPath))
		if !isSidecarExt(ext) {
			continue
		}
		// stem is the sidecar path without its subtitle extension.
		stem := strings.TrimSuffix(relPath, ext)
		if lang, ok := sidecarLangForBase(stem, base, mediaExt); ok {
			exact = append(exact, sidecarFile{RelPath: relPath, Lang: lang, Ext: ext, MTimeUnix: mtime})
			continue
		}
		if variantBase == "" {
			continue
		}
		// The normalized base is lower-cased (it is a §8.6.5 grouping key), so the
		// stem is lower-cased for this comparison only. The recorded language token
		// is therefore lower-case too, which §9.5 matches case-insensitively.
		if lang, ok := sidecarLangForBase(strings.ToLower(stem), variantBase, mediaExt); ok {
			variant = append(variant, sidecarFile{RelPath: relPath, Lang: lang, Ext: ext, MTimeUnix: mtime})
		}
	}
	out := mergeSidecarCandidates(exact, variant)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lang != out[j].Lang {
			return out[i].Lang < out[j].Lang
		}
		if out[i].Ext != out[j].Ext {
			return out[i].Ext < out[j].Ext
		}
		return out[i].RelPath < out[j].RelPath
	})
	return out
}

// sidecarLangForBase reports whether a sidecar stem (its path without the
// subtitle extension) binds to the given media base, and returns the language
// token it declares. tail is the remainder after the base; only the two
// documented §8.6.4 shapes bind:
//
//	"<base><ext>"        → tail == ""        (no language)
//	"<base>.<lang><ext>" → tail == ".<lang>" (single token, no extra dots)
//
// Anything else — an extra dotted segment ("clip.notes.en.vtt") or the media
// extension reused as the token ("clip.mp3.vtt") for media "clip.mp3" — is NOT
// this media's sidecar and is rejected, so it cannot wrongly suppress STT. Both
// candidate bases run through this one function, so every guard below applies to
// the normalized base exactly as it applies to the exact base.
func sidecarLangForBase(stem, base, mediaExt string) (string, bool) {
	if !strings.HasPrefix(stem, base) {
		return "", false
	}
	tail := strings.TrimPrefix(stem, base)
	switch {
	case tail == "":
		// undifferentiated sidecar, no language tag
		return "", true
	case strings.HasPrefix(tail, ".") && !strings.Contains(tail[1:], "."):
		lang := strings.TrimSpace(tail[1:])
		if lang == "" {
			return "", false
		}
		if mediaExt != "" && strings.EqualFold(lang, mediaExt) {
			// e.g. "clip.mp3.vtt" for media "clip.mp3": the token is the media's
			// own extension, not a language tag.
			return "", false
		}
		if !isKnownLanguageTag(lang) {
			// The single tail token is not a real language tag but a stray filename
			// fragment — "clip.HD.vtt" (token "HD"), "clip.2024.vtt" (token "2024"),
			// or a cross-media extension "clip.mp4.vtt" bound to "clip.mp3" (token
			// "mp4"). Binding such a token would record a bogus language
			// ("HD"/"2024"/"mp4") AND suppress real STT, so it is not this media's
			// sidecar and is rejected (issue #431 §8.6.4).
			return "", false
		}
		return lang, true
	default:
		return "", false
	}
}

// variantSidecarBase returns the second candidate sidecar base for a media path:
// its §8.6.5 normalized name (rendition markers stripped) without the extension.
// It returns "" when there is no usable second candidate, so the caller runs the
// exact pass alone.
func variantSidecarBase(mediaRelPath, exactBase string) string {
	// Only a media rendition has a rendition marker to strip; §8.6.5 grouping is
	// scoped to the same extension set.
	if !isMediaVariantFile(mediaRelPath) {
		return ""
	}
	normalized := stripExt(normalizeVariantName(mediaRelPath))
	// The stem collapsed entirely into rendition markers ("1080p.mp4"), leaving an
	// empty base or a bare directory. Such a base is a prefix of every sidecar in
	// the corpus (or in that directory), so it is refused rather than bound.
	if normalized == "" || strings.HasSuffix(normalized, "/") {
		return ""
	}
	// No marker was stripped: the normalized base differs from the exact base only
	// in case (normalizeVariantName lower-cases its key). The exact pass already
	// covers it, and a case-only second pass would bind a genuinely different
	// file's sidecar on a case-sensitive filesystem.
	if strings.EqualFold(normalized, exactBase) {
		return ""
	}
	return normalized
}

// mergeSidecarCandidates combines the exact-base and normalized-base matches
// under two precedence rules. Both exist to stop the inferred (normalized) base
// from overriding, or duplicating, what the exact base already states:
//
//  1. An exact-base sidecar WINS for its language: a normalized-base sidecar of
//     the same language is dropped. Without this, "<sha>_1080p.en.vtt" and
//     "<sha>.en.vtt" would both feed the "en" transcript and duplicate its cues.
//  2. An UNTAGGED normalized-base sidecar ("<sha>.ttml", no language token)
//     binds ONLY when no language-tagged sidecar binds to this media. The exact
//     base is an explicit statement of intent, but a normalized base is
//     inferred, so an untagged file found there declares neither which work nor
//     which language it belongs to. A TTML additionally carries its own xml:lang
//     internally, so binding it beside an authored "<sha>.ru.vtt" would silently
//     append a second copy of the same language's cues. When it is the only
//     candidate it still binds, so a lone untagged sidecar is never lost.
//
// Rule 2 applies to the normalized pass only; an exact-base untagged sidecar is
// unaffected. An operator who wants a TTML ingested next to per-language VTTs
// names it "<sha>.<lang>.ttml", which binds under rule 1.
func mergeSidecarCandidates(exact, variant []sidecarFile) []sidecarFile {
	if len(variant) == 0 {
		return exact
	}
	exactLangs := make(map[string]struct{}, len(exact))
	tagged := false
	for _, sc := range exact {
		exactLangs[strings.ToLower(sc.Lang)] = struct{}{}
		if sc.Lang != "" {
			tagged = true
		}
	}
	for _, sc := range variant {
		if sc.Lang != "" {
			tagged = true
		}
	}
	out := make([]sidecarFile, 0, len(exact)+len(variant))
	out = append(out, exact...)
	for _, sc := range variant {
		if _, ok := exactLangs[strings.ToLower(sc.Lang)]; ok {
			continue // rule 1: the exact base already supplies this language
		}
		if sc.Lang == "" && tagged {
			continue // rule 2: an untagged inferred sidecar yields to a tagged one
		}
		out = append(out, sc)
	}
	return out
}

// sidecarIndexOrWalk returns the scan-built sidecar index, falling back to a
// one-shot corpus walk when none was set (direct/standalone calls). The walk
// result is not cached so a standalone call always sees current mtimes.
func (s *Service) sidecarIndexOrWalk(ctx context.Context) map[string]int64 {
	s.sidecarMu.RLock()
	idx := s.sidecarIndex
	s.sidecarMu.RUnlock()
	if idx != nil {
		return idx
	}
	files, err := s.corpusFS().Walk(ctx, s.cfg.RootDir, DiscoverOptionsFromConfig(s.cfg).corpusfsOptions())
	if err != nil {
		s.getLogger().Printf("sidecar walk for %s failed: %v", s.cfg.RootDir, err)
		return nil
	}
	out := make(map[string]int64, len(files))
	for _, f := range files {
		// Honour PathExcludes here too so excluded files (e.g. "**/*.vtt") are
		// never used as sidecars on the standalone fallback path.
		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			continue
		}
		out[f.RelPath] = f.MTimeUnix
	}
	return out
}

// sidecarFingerprint returns a stable fingerprint of the media document's
// sidecars (their sorted rel_paths and mtimes). Folding it into the media
// document's content hash makes the incremental gate (§7.6) re-process the media
// whenever a sidecar is added, removed, or modified — even though the media
// bytes are unchanged. Returns "" when sidecars are disabled or the document is
// not a media type with any sidecar.
func (s *Service) sidecarFingerprint(ctx context.Context, relPath, docType string) string {
	if !s.sidecarsEnabled() || !isSidecarMediaType(docType) {
		return ""
	}
	sidecars := s.findSidecars(ctx, relPath)
	if len(sidecars) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sidecars))
	for _, sc := range sidecars {
		parts = append(parts, fmt.Sprintf("%s@%d", sc.RelPath, sc.MTimeUnix))
	}
	return strings.Join(parts, "\n")
}

// ingestSidecarTranscripts ingests the media document's subtitle sidecars as its
// transcript representation(s), one representation per language, and reports
// whether any were ingested. When it returns true the caller MUST skip STT — an
// authored sidecar is authoritative over machine transcription (§8.6.4). Sidecar
// transcripts bypass the output quality gate: they are authored text, not
// model-derived output, so quarantining them as degenerate model output would be
// wrong (§8.6.6/§8.6.7). A parse failure for one sidecar is logged and skipped;
// remaining sidecars (and, if none parse, STT) still proceed.
func (s *Service) ingestSidecarTranscripts(ctx context.Context, doc model.Document) (bool, error) {
	if !s.sidecarsEnabled() || !isSidecarMediaType(doc.DocType) {
		return false, nil
	}
	groups := s.collectSidecarCues(ctx, doc)
	if len(groups) == 0 {
		return false, nil
	}
	langs := make([]string, 0, len(groups))
	for lang := range groups {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	// Build the cleaning options once for all languages; they are config-derived
	// and language-independent. They apply the configured media.subtitles.*
	// cleaning to the chunk text before embedding (issues #545, #765), the same
	// cleaning the export path applies to the sidecar. Off by default (inactive
	// options = no-op). The collapse-repeats run counter stays per language: each
	// applyCueCleaningToSegments call builds a fresh collapser, so one language's
	// trailing cue can never start a run in the next.
	cleanOpts := s.captionCleanOptions()

	// Chunk every language BEFORE persisting any of them, so the #681 secret screen
	// below sees the whole document's sidecar text at once. Screening per language
	// inside the persist loop would let a clean first language commit (and count a
	// representation) before a later language withheld the document and retired it
	// again, which would leave the run's representation counter describing a
	// representation that no longer exists.
	perLang := make(map[string][]chunkSegment, len(langs))
	var allText strings.Builder
	for _, lang := range langs {
		segments := chunkSubtitleCuesFiltered(groups[lang], s.captionWordFilter())
		segments = applyCueCleaningToSegments(segments, cleanOpts)
		if len(segments) == 0 {
			continue
		}
		perLang[lang] = segments
		if allText.Len() > 0 {
			allText.WriteByte('\n')
		}
		allText.WriteString(joinSegmentTexts(segments))
	}
	// #681: a sidecar becomes the media document's searchable transcript, so its
	// cue text is screened exactly like an STT transcript, over the cleaned strings
	// that are chunked and embedded. On a match the document is already withheld
	// and nothing below runs.
	if s.screenDerivedSecrets(ctx, doc, derivedKindSidecar, allText.String()) {
		return false, nil
	}

	ingested := false
	for _, lang := range langs {
		segments, ok := perLang[lang]
		if !ok {
			continue
		}
		if err := s.persistSidecarTranscript(ctx, doc, lang, groups[lang], segments); err != nil {
			return ingested, err
		}
		s.addRepresentations(1)
		ingested = true
	}
	return ingested, nil
}

// collectSidecarCues parses every sidecar of the media document and groups the
// resulting cues by language. A per-language sidecar (clip.en.vtt) contributes
// to its own language; a bilingual TTML contributes to each xml:lang it carries.
// Each language's cues are sorted by start time for stable, deterministic spans.
func (s *Service) collectSidecarCues(ctx context.Context, doc model.Document) map[string][]subtitle.Cue {
	groups := map[string][]subtitle.Cue{}
	for _, sc := range s.findSidecars(ctx, doc.RelPath) {
		content, err := s.readSidecar(ctx, sc.RelPath)
		if err != nil {
			s.getLogger().Printf("sidecar read %s: %v", sc.RelPath, err)
			continue
		}
		for lang, cues := range parseSidecar(sc, content) {
			groups[lang] = append(groups[lang], cues...)
		}
	}
	for lang := range groups {
		cues := groups[lang]
		sort.SliceStable(cues, func(i, j int) bool {
			if cues[i].StartMS != cues[j].StartMS {
				return cues[i].StartMS < cues[j].StartMS
			}
			return cues[i].EndMS < cues[j].EndMS
		})
		groups[lang] = cues
	}
	return groups
}

// parseSidecar parses one sidecar file into language-keyed cue sets. VTT/SRT
// carry a single (file-level) language taken from the filename; TTML may be
// bilingual and is split per xml:lang, with the filename language used as a
// fallback for any cues that declare none.
func parseSidecar(sc sidecarFile, content string) map[string][]subtitle.Cue {
	out := map[string][]subtitle.Cue{}
	switch sc.Ext {
	case ".ttml":
		groups, err := subtitle.ParseTTMLByLang(content)
		if err != nil {
			return out
		}
		for _, g := range groups {
			lang := g.Lang
			if lang == "" {
				lang = sc.Lang
			}
			out[lang] = append(out[lang], g.Cues...)
		}
	case ".srt":
		if cues, err := subtitle.ParseSRT(content); err == nil {
			out[sc.Lang] = cues
		}
	default: // .vtt
		if cues, err := subtitle.ParseVTT(content); err == nil {
			out[sc.Lang] = cues
		}
	}
	return out
}

// persistSidecarTranscript writes one language's sidecar cues as a transcript
// representation plus its time-spanned chunks. The chunks are inserted healthy
// (no quarantine decision) so they embed normally; sidecar text is never
// screened by the quality gate (§8.6.6/§8.6.7). meta_json records source=sidecar
// and the language so retrieval and re-derivation treat it as authored.
func (s *Service) persistSidecarTranscript(ctx context.Context, doc model.Document, lang string, cues []subtitle.Cue, segments []chunkSegment) error {
	// Normalize once so the recorded language and its provenance are derived from
	// the same value (no padded/non-canonical language stored alongside a trimmed
	// provenance decision).
	lang = strings.TrimSpace(lang)
	meta := transcriptMeta{Source: sidecarSource, Language: lang, Timestamps: true}
	// A sidecar's language is asserted by the source itself — the filename suffix
	// (clip.en.vtt §8.6.4) or a TTML xml:lang — so its provenance is "declared"
	// (SPEC §5.2/§8.8). An undifferentiated sidecar (clip.vtt) carries no language
	// tag: lang is empty (unknown), so we leave language_source unset rather than
	// asserting a declaration that was never made.
	if lang != "" {
		meta.LanguageSource = langSourceDeclared
	}
	// §8.6.9: declare word-level granularity when a cue carried per-word timing
	// (omitempty ⇒ a segment-only sidecar's meta_json is unchanged). Cue-based
	// sidecars are usually segment-only, so this is normally false/absent.
	meta.Words = segmentsHaveWordTiming(segments)
	// A sidecar that carried <v> voice tags yields speaker-attributed segments
	// (SPEC §8.6.8). Such a transcript is diarized WITHOUT a model, so it records
	// diarized:true and the speakers set but NO diarize_provider/model (mirrors
	// §8.6.7 for authored sources). A sidecar with no voice tags is unchanged.
	if speakers := distinctSpeakers(segments); len(speakers) > 0 {
		meta.Diarized = true
		meta.Speakers = speakers
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal sidecar transcript meta: %w", err)
	}
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     TranscriptRepType(lang),
		RepHash:     sidecarRepHash(lang, cues),
		MetaJSON:    string(metaJSON),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert sidecar transcript representation: %w", err)
	}
	// quarantineDecision{} = insert healthy chunks; sidecar text bypasses the gate.
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, quarantineDecision{}); err != nil {
		return fmt.Errorf("persist sidecar transcript chunks: %w", err)
	}
	return nil
}

// sidecarRepHash derives a stable rep_hash from the language and cue content so
// an unchanged sidecar dedups across re-ingests and a changed one produces a new
// representation identity.
func sidecarRepHash(lang string, cues []subtitle.Cue) string {
	var b strings.Builder
	b.WriteString(lang)
	b.WriteByte('\n')
	for _, c := range cues {
		// Fold the voice/speaker name into the hash so a sidecar edit that changes
		// ONLY a <v Name> tag (which is stripped from the cue text) still yields a
		// new representation identity and re-derives the attribution (SPEC §8.6.8).
		fmt.Fprintf(&b, "%d-%d:%s\t%s\n", c.StartMS, c.EndMS, c.Speaker, c.Text)
	}
	return computeRepHash([]byte(b.String()))
}

// readSidecar reads a sidecar file's bytes through the corpus FS and normalises
// them to valid UTF-8 with LF line endings.
//
// The read is bounded by the configured cap (#682). A sidecar is a corpus file
// like any other, so it is subject to the same cap, and it reaches this read
// through the sidecar index rather than through the document pipeline. An
// over-cap sidecar returns ErrFileTooLarge instead of a truncated cue list: half
// a subtitle file would be persisted as if it were the whole transcript.
func (s *Service) readSidecar(ctx context.Context, relPath string) (string, error) {
	raw, overCap, err := s.readSourceBytes(ctx, relPath)
	if err != nil {
		return "", err
	}
	if overCap {
		return "", s.sourceOverCapError(relPath)
	}
	return string(NormalizeUTF8(raw)), nil
}

// IngestSidecarTranscripts exposes sidecar transcript ingestion for tests. It
// returns whether any sidecar was ingested (true means STT should be skipped).
func (s *Service) IngestSidecarTranscripts(ctx context.Context, doc model.Document) (bool, error) {
	return s.ingestSidecarTranscripts(ctx, doc)
}

// isSidecarMediaType reports whether a sidecar transcript may stand in for STT
// on this doc type. Audio and video are the media types that carry transcripts
// (§8.6.4).
func isSidecarMediaType(docType string) bool {
	return docType == "audio" || docType == "video"
}

// isKnownLanguageTag reports whether token is a real BCP-47 language tag whose
// primary subtag is a registered ISO 639 language, not merely a syntactically
// well-formed string. It guards the sidecar language-suffix binding (§8.6.4): a
// dot-free tail token is only treated as a language tag when it actually names a
// language, so a stray filename fragment ("HD", "2024") or a cross-media
// extension token ("mp4" for a "clip.mp3" asset) is rejected rather than bound
// as a bogus-language transcript that also suppresses real STT (issue #431).
//
// Validation is on the PRIMARY subtag (matching the §9.5 primary-subtag
// contract), so region/script/variant-tagged sidecars still bind on their base
// language: "pt-BR", "zh-Hant", and "en-orig" all validate via base "pt"/"zh"/
// "en". The dependency on golang.org/x/text/language is confined to this ingest
// helper; model.LanguagePrimarySubtag/IsValidLanguageTag stay parser-free.
func isKnownLanguageTag(token string) bool {
	if !model.IsValidLanguageTag(token) {
		return false
	}
	// ParseBase validates the primary subtag against the ISO 639 registry: it
	// returns an error for a well-formed-but-unknown subtag ("hd") and for a
	// not-well-formed one ("mp4"/"2024"), while accepting real codes ("en", "ru",
	// "pt", "cmn", ...). The confidence is irrelevant; only the error matters.
	_, err := language.ParseBase(model.LanguagePrimarySubtag(token))
	return err == nil
}

// isSidecarExt reports whether ext (lowercased, with leading dot) is a
// recognised subtitle sidecar extension.
func isSidecarExt(ext string) bool {
	for _, e := range sidecarExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// stripExt removes the final extension from a path, preserving directories.
func stripExt(p string) string {
	return strings.TrimSuffix(p, path.Ext(p))
}
