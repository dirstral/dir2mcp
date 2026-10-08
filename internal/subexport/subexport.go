// Package subexport renders a stored transcript as a subtitle document. It is
// the ONE place a transcript becomes VTT, SRT or TTML bytes, shared by the
// `export` command (SPEC §8.6.3/§8.6.10) and by ingest-time write-back (SPEC
// §8.6.14), so the file written beside a media document is byte-identical to
// the file `export` would produce for it under the same configuration.
//
// Everything here is deterministic: the same representations, chunks and
// configuration always render the same bytes.
package subexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// Store is the read surface a renderer needs: the transcript representations of
// a document and the time-spanned chunks of one representation. The SQLite store
// satisfies it; tests may supply a fake.
type Store interface {
	TranscriptRepresentations(ctx context.Context, relPath string) ([]store.TranscriptRepresentation, error)
	TranscriptSpanChunks(ctx context.Context, repID int64) ([]store.TranscriptSpanChunk, error)
}

// Sentinel errors. Callers map them onto their own error surface (the CLI keeps
// its historical messages and exit codes); errors.Is works through the wrapping
// the renderer adds.
var (
	// ErrNoDocument: no document exists at the requested path.
	ErrNoDocument = errors.New("no document at path")
	// ErrNoTranscript: the document exists but has no transcript representation.
	ErrNoTranscript = errors.New("document has no transcript representation")
	// ErrNoLanguage: the document has transcripts, none in the requested language.
	ErrNoLanguage = errors.New("document has no transcript for language")
	// ErrNoCues: the selected transcript has no time-coded cues to render.
	ErrNoCues = errors.New("transcript has no time-coded cues")
)

// Pipeline is the single cue-preparation pipeline every subtitle format renders
// through: the media.filter_words word filter followed by the media.subtitles.*
// editorial cleaning passes (glossary, drop_phrases, scrub_phrases,
// collapse_repeats, drop_urls, expect_script).
//
// Five of the six cleaning keys (drop_urls, expect_script, drop_phrases,
// scrub_phrases, collapse_repeats) are ALSO applied at ingest, before chunks are embedded
// (issues #545, #765), from the same subtitle.CleanOptions shape this builds.
// Re-running them here is deliberate rather than redundant: under broadcast
// segmentation the cues are rebuilt finer than the stored chunks, and a corpus
// indexed before the keys were enabled still carries uncleaned chunks until it
// is re-indexed. Only glossary is export-only (SPEC §8.6.2).
//
// Formats differ in how cues are BUILT (segmentation, bilingual alignment) but
// MUST NOT differ in how cues are CLEANED (issue #729), so the cleaning half
// lives here and every renderer calls Apply.
type Pipeline struct {
	filter *subtitle.WordFilter
	clean  subtitle.CleanOptions
}

// NewPipeline compiles the configured filter/glossary/drop/scrub rules. The
// config loader already validated all of them, so a compile error here is
// unexpected; it is returned rather than silently degrading into a no-op
// pipeline that would ship uncleaned cues.
func NewPipeline(cfg config.Config) (Pipeline, error) {
	glossary, err := subtitle.NewGlossary(cfg.MediaSubtitlesGlossary)
	if err != nil {
		return Pipeline{}, fmt.Errorf("invalid media.subtitles.glossary: %w", err)
	}
	drop, err := subtitle.NewDropSet(cfg.MediaSubtitlesDropPhrases)
	if err != nil {
		return Pipeline{}, fmt.Errorf("invalid media.subtitles.drop_phrases: %w", err)
	}
	scrub, err := subtitle.NewDropSet(cfg.MediaSubtitlesScrubPhrases)
	if err != nil {
		return Pipeline{}, fmt.Errorf("invalid media.subtitles.scrub_phrases: %w", err)
	}
	script, err := subtitle.NewScriptGuard(cfg.MediaSubtitlesExpectScript)
	if err != nil {
		return Pipeline{}, fmt.Errorf("invalid media.subtitles.expect_script: %w", err)
	}
	return Pipeline{
		filter: subtitle.NewWordFilter(cfg.MediaFilterWords),
		clean: subtitle.CleanOptions{
			DropURLs:        cfg.MediaSubtitlesDropURLs,
			Script:          script,
			Drop:            drop,
			Scrub:           scrub,
			CollapseRepeats: cfg.MediaSubtitlesCollapseRepeats,
			Glossary:        glossary,
		},
	}, nil
}

// Apply runs the word filter then the cleaning passes over built cues, in that
// order, so filter_words removal and the cleanup compose (a cue emptied by the
// filter is dropped before the cleaning passes ever see it). An empty config
// leaves cues unchanged, so enabling nothing changes nothing.
func (p Pipeline) Apply(cues []subtitle.Cue) []subtitle.Cue {
	cues = subtitle.FilterCues(cues, p.filter)
	return subtitle.CleanCues(cues, p.clean)
}

// SelectRep chooses the transcript representation to render. With an empty lang
// it returns the first (source/only) transcript — there is no hardcoded default
// language. With a non-empty lang it returns the first representation whose
// meta_json language matches (case-insensitively), reporting ok=false when none
// match.
func SelectRep(reps []store.TranscriptRepresentation, lang string) (store.TranscriptRepresentation, bool) {
	if len(reps) == 0 {
		return store.TranscriptRepresentation{}, false
	}
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return reps[0], true
	}
	for _, rep := range reps {
		if strings.EqualFold(RepLanguage(rep.MetaJSON), lang) {
			return rep, true
		}
	}
	return store.TranscriptRepresentation{}, false
}

// RepLanguage extracts a language code from a transcript representation's
// meta_json, accepting either a "language" or "lang" key. Returns "" when the
// metadata is absent or unparseable, so language-tagged sidecar transcripts
// (#253) match while legacy untagged ones simply do not.
func RepLanguage(metaJSON string) string {
	meta, ok := parseMeta(metaJSON)
	if !ok {
		return ""
	}
	for _, key := range []string{"language", "lang"} {
		if v, ok := meta[key]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// RepIsTranslation reports whether a transcript representation's meta_json marks
// it as a machine translation (source == "translation", set by the ingest
// translate path). Broadcast export always reflows a translation rather than
// honoring its fabricated per-word timings; see BuildCuesForSegmentation. It
// fails closed: non-empty but unparseable meta_json is treated as a translation
// so a corrupt rep can never route fabricated timings into the broadcast path.
func RepIsTranslation(metaJSON string) bool {
	if strings.TrimSpace(metaJSON) == "" {
		return false
	}
	meta, ok := parseMeta(metaJSON)
	if !ok {
		// Fail closed: meta_json is present but unparseable, so we cannot confirm
		// this is a native transcript with real per-word timings. Reflowing a
		// native transcript merely trades re-segmentation for safe timing, whereas
		// honoring fabricated translation timings is a correctness bug.
		return true
	}
	// A missing "source" key is the normal native-transcript case (only the
	// translate path sets source="translation"; native reps carry language meta
	// but no source), so absence means native — not translation.
	return repSource(meta) == "translation"
}

// RepIsSidecar reports whether a transcript representation was ingested from an
// authored subtitle sidecar (SPEC §8.6.4) rather than derived by a model.
func RepIsSidecar(metaJSON string) bool {
	meta, ok := parseMeta(metaJSON)
	return ok && repSource(meta) == "sidecar"
}

func parseMeta(metaJSON string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(metaJSON)
	if trimmed == "" {
		return nil, false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
		return nil, false
	}
	return meta, true
}

func repSource(meta map[string]any) string {
	src, _ := meta["source"].(string)
	return strings.ToLower(strings.TrimSpace(src))
}

// BuildCuesForSegmentation selects the cue builder by the configured
// media.subtitles.segmentation mode: "broadcast" re-segments into broadcast-
// legible cues; any other value (including the "chunk" default and empty) uses
// BuildCues, so the historical behavior is unchanged unless broadcast is
// explicitly selected.
//
// In broadcast mode the source of the timing matters:
//   - A native STT transcript that carries real per-word timings re-segments
//     from them (BuildBroadcastCues).
//   - A translation (isTranslation) is always reflowed. Any per-word timings a
//     translate provider emits are FABRICATED — words pile at a single timestamp
//     with zero duration — so honoring them would cram a whole clause into a
//     sub-second cue and spike reading speed. ReflowChunkCues instead distributes
//     each run's on-screen time across its tokens. This gates on the source, not
//     on timings-present, so a future translate provider that emits word timings
//     can never bypass the reflow.
//   - A native transcript with no per-word timings (BuildBroadcastCues returns
//     nil) is reflowed too.
func BuildCuesForSegmentation(chunks []subtitle.TranscriptChunk, segmentation string, isTranslation bool) []subtitle.Cue {
	if strings.EqualFold(strings.TrimSpace(segmentation), "broadcast") {
		if !isTranslation {
			if cues := subtitle.BuildBroadcastCues(chunks); cues != nil {
				return cues
			}
		}
		return subtitle.ReflowChunkCues(subtitle.BuildCues(chunks))
	}
	return subtitle.BuildCues(chunks)
}

// Renderer renders a document's stored transcripts under one configuration.
type Renderer struct {
	cfg  config.Config
	pipe Pipeline
}

// NewRenderer compiles the cue pipeline for cfg once so a caller rendering many
// documents (ingest write-back) does not recompile the rules per document.
func NewRenderer(cfg config.Config) (Renderer, error) {
	pipe, err := NewPipeline(cfg)
	if err != nil {
		return Renderer{}, err
	}
	return Renderer{cfg: cfg, pipe: pipe}, nil
}

// Pipeline returns the compiled cue pipeline.
func (r Renderer) Pipeline() Pipeline { return r.pipe }

// Cues resolves the transcript of relPath in lang (empty = the document's
// first/source transcript), builds its cues under the given segmentation mode
// and runs them through the pipeline. It returns the cues and the RESOLVED
// language recorded on the representation (which may differ from the requested
// tag in case, or be empty for an untagged transcript).
func (r Renderer) Cues(ctx context.Context, st Store, relPath, lang, segmentation string) ([]subtitle.Cue, string, error) {
	reps, err := st.TranscriptRepresentations(ctx, relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("%w: %q", ErrNoDocument, relPath)
		}
		return nil, "", fmt.Errorf("load transcript representations: %w", err)
	}
	if len(reps) == 0 {
		return nil, "", fmt.Errorf("%w: %q", ErrNoTranscript, relPath)
	}
	rep, ok := SelectRep(reps, lang)
	if !ok {
		return nil, "", fmt.Errorf("%w: %q lang %q", ErrNoLanguage, relPath, lang)
	}
	cues, err := r.cuesForRep(ctx, st, rep, segmentation)
	if err != nil {
		return nil, "", err
	}
	return cues, RepLanguage(rep.MetaJSON), nil
}

// cuesForRep loads one representation's chunks and builds its cleaned cues.
func (r Renderer) cuesForRep(ctx context.Context, st Store, rep store.TranscriptRepresentation, segmentation string) ([]subtitle.Cue, error) {
	rows, err := st.TranscriptSpanChunks(ctx, rep.RepID)
	if err != nil {
		return nil, fmt.Errorf("load transcript chunks: %w", err)
	}
	chunks := make([]subtitle.TranscriptChunk, 0, len(rows))
	for _, row := range rows {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: row.Text, Span: row.Span})
	}
	return r.pipe.Apply(BuildCuesForSegmentation(chunks, segmentation, RepIsTranslation(rep.MetaJSON))), nil
}

// RenderTimed renders the transcript of relPath in lang as "vtt" or "srt" under
// the configured media.subtitles.segmentation. ErrNoCues is returned when the
// transcript has nothing time-coded to render.
func (r Renderer) RenderTimed(ctx context.Context, st Store, relPath, lang, format string) (string, error) {
	cues, _, err := r.Cues(ctx, st, relPath, lang, r.cfg.MediaSubtitlesSegmentation)
	if err != nil {
		return "", err
	}
	if len(cues) == 0 {
		return "", fmt.Errorf("%w: %q", ErrNoCues, relPath)
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "vtt":
		return subtitle.RenderVTT(cues), nil
	case "srt":
		return subtitle.RenderSRT(cues), nil
	default:
		return "", fmt.Errorf("unsupported timed subtitle format %q (want vtt|srt)", format)
	}
}

// RenderTTML renders a (bilingual) TTML document (SPEC §8.6.10) for relPath.
// primaryLang selects the primary transcript (empty = the document's first);
// secondaryLang, when non-empty, selects the transcript aligned onto the primary
// cues within media.subtitles.ttml.align_tolerance_ms. TTML cues are always built
// in "chunk" segmentation: broadcast re-segmentation is a VTT/SRT reading-speed
// concern and is documented as never affecting TTML. The resolved primary
// language is returned alongside the document so a caller can tag companions
// (the SMIL sidecar) with the same language the TTML carries.
func (r Renderer) RenderTTML(ctx context.Context, st Store, relPath, primaryLang, secondaryLang string) (ttml string, resolvedPrimary string, err error) {
	primary, resolved, err := r.Cues(ctx, st, relPath, primaryLang, "chunk")
	if err != nil {
		return "", "", err
	}
	var bilingual []subtitle.BilingualCue
	if strings.TrimSpace(secondaryLang) != "" {
		secondary, resolvedSecondary, serr := r.Cues(ctx, st, relPath, secondaryLang, "chunk")
		if serr != nil {
			return "", "", serr
		}
		bilingual = subtitle.AlignBilingual(primary, secondary, resolved, resolvedSecondary,
			r.cfg.MediaSubtitlesTTMLAlignToleranceMS)
	} else {
		bilingual = subtitle.MonolingualBilingualCues(primary, resolved)
	}
	if len(bilingual) == 0 {
		return "", "", fmt.Errorf("%w: %q", ErrNoCues, relPath)
	}
	return subtitle.RenderTTML(bilingual, resolved), resolved, nil
}
