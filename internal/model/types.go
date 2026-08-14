package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Document struct {
	DocID       int64
	RelPath     string
	Title       string
	DocType     string
	SourceType  string
	SizeBytes   int64
	MTimeUnix   int64
	ContentHash string
	// ETag is the remote backend's cheap change token (S3 object ETag) when the
	// document was discovered from an object store. It is empty for local/NFS
	// corpora. The ETag MUST NOT be treated as a content hash (multipart and
	// SSE-KMS ETags are not MD5 of the body); it only decides whether a re-read
	// + content_hash recompute is warranted (SPEC §7.8.3). content_hash stays the
	// canonical identity.
	ETag string
	// SidecarFingerprint is a stable, cheaply-recomputed signature of the media
	// document's adjacent subtitle sidecars (their sorted rel_paths + mtimes). It
	// is the SAME value folded into ContentHash on the full read+hash path,
	// persisted separately so the remote (S3) ETag fast path can detect a sidecar
	// added/changed/removed while the media object's own ETag is unchanged —
	// without re-reading the media bytes (SPEC §7.8.3, #298). Empty for non-media
	// documents and media with no sidecar.
	SidecarFingerprint string
	Status             string
	Deleted            bool
	// ErrorMessage is populated when Status == "error" with a short
	// human-readable description of why ingest failed (extraction
	// crash, representation generation failure, etc.). Surfaced in
	// the support bundle's list-files.json so a maintainer can tell
	// *why* a document failed without having to grep server.log. The
	// field is intentionally not part of the MCP `list_files` tool
	// output (that schema is fixed by SPEC §15.5 with
	// additionalProperties:false); it is a diagnostic-bundle field.
	ErrorMessage string
	// SkipReason is the stable classification of *why* a document was
	// recorded as skipped (never indexed) rather than ingested — one of
	// the SkipReason* constants below. Empty for ingested ("ok") and
	// errored documents; populated only on status="skipped" /
	// "secret_excluded" rows. Aggregated into CorpusStats.SkipSummary so
	// `status`/`reindex` can report honest coverage ("what wasn't indexed
	// & why", #414/#395). It is a plain string (mirroring
	// FailureSummary.Categories) so the model package does not depend on
	// internal/store.
	SkipReason string
}

// SkipReason* enumerate the stable reasons a discovered file is recorded as
// skipped (never indexed) rather than ingested or errored. Persisted in
// documents.skip_reason and grouped by CorpusStats.SkipSummary. Kept as plain
// string constants so callers across packages share one vocabulary without an
// import cycle back to internal/store.
const (
	// SkipReasonUnsupportedFormat: the file's format has no configured
	// extractor/OCR/transcriber path (e.g. .odt/.rtf without a reader).
	SkipReasonUnsupportedFormat = "unsupported_format"
	// SkipReasonBinaryIgnored: a binary artifact classified as non-textual
	// and deliberately not ingested.
	SkipReasonBinaryIgnored = "binary_ignored"
	// SkipReasonArchive: an archive container persisted as a skipped row; its
	// members are extracted separately and are not directly indexable.
	SkipReasonArchive = "archive"
	// SkipReasonIgnoreRule: a file matched a built-in ignore classification
	// (e.g. .env variants) and was excluded from the pipeline.
	SkipReasonIgnoreRule = "ignore_rule"
	// SkipReasonSecretExcluded: content matched a secret pattern, so the file
	// was excluded from ingestion (status="secret_excluded").
	SkipReasonSecretExcluded = "secret_excluded"
	// SkipReasonPathExcluded: the rel_path matched a configured path-exclude
	// glob and was dropped at scan time (no durable row is persisted).
	SkipReasonPathExcluded = "path_excluded"
	// SkipReasonSizeCap: the file exceeded ingest.max_file_mb and was dropped
	// at discovery.
	SkipReasonSizeCap = "size_cap"
	// SkipReasonSymlinkIgnored: a discovered entry is a symbolic link and
	// ingest.follow_symlinks is false, so the walker does not follow the link
	// and does not index the target. It covers a link to a file and a link to a
	// directory alike: with following off the walker never resolves the target,
	// so it cannot tell the two apart (SPEC §7.1/§15.2, spec 0.46.0).
	SkipReasonSymlinkIgnored = "symlink_ignored"
	// SkipReasonLanguageUncovered: media whose resolved source language is outside
	// the selected STT model's declared stt_languages coverage, skipped under
	// media.stt.on_uncovered_language=skip (SPEC §8.2.1) instead of transcribed to
	// degraded output. No transcript representation is produced.
	SkipReasonLanguageUncovered = "language_uncovered"
)

type Representation struct {
	RepID       int64
	DocID       int64
	RepType     string
	RepHash     string
	MetaJSON    string
	CreatedUnix int64
	Deleted     bool
}

// SummaryRepType is the rep_type of a model-generated summary representation
// (SPEC §5.2, hierarchical retrieval §9.7). A summary is a coarse view over the
// fine chunks of exactly ONE source representation of its OWN document; it
// retrieves but never cites (§9.7 citation-faithfulness invariant).
const SummaryRepType = "summary"

// IsSummaryRepType reports whether repType is a summary representation: the
// canonical `summary`, or a `summary-<source_rep_type>` variant written when one
// document is summarized at more than one source representation (the same
// suffixing idiom per-language `transcript-<lang>` representations use). It is
// the single predicate every layer uses so a summary can never be mistaken for a
// citable fine chunk (§9.7).
func IsSummaryRepType(repType string) bool {
	repType = strings.ToLower(strings.TrimSpace(repType))
	return repType == SummaryRepType || strings.HasPrefix(repType, SummaryRepType+"-")
}

// Summary coverage range kinds (SPEC §5.2). `document` covers every non-deleted
// chunk of the source representation; `ordinals` covers an INCLUSIVE chunk
// ordinal range; `time` covers transcript segments/clips by interval OVERLAP.
const (
	SummaryRangeDocument = "document"
	SummaryRangeOrdinals = "ordinals"
	SummaryRangeTime     = "time"
)

// Summary levels (SPEC §5.2/§16.2): one summary over the whole document, or one
// over a deterministic window of N adjacent fine units.
const (
	SummaryLevelDocument = "document"
	SummaryLevelSection  = "section"
)

// SummaryCoverageRange is the fine-unit range a summary covers within its source
// representation (SPEC §5.2). Exactly one shape is populated, selected by Kind:
//
//   - SummaryRangeDocument: no bounds — every non-deleted chunk of the source rep.
//   - SummaryRangeOrdinals: [Start, End], an INCLUSIVE chunk-ordinal range.
//   - SummaryRangeTime: [StartMS, EndMS], an INCLUSIVE time range matched by
//     interval OVERLAP against each segment's [seg_start_ms, seg_end_ms].
type SummaryCoverageRange struct {
	Kind    string `json:"kind"`
	Start   int    `json:"start,omitempty"`
	End     int    `json:"end,omitempty"`
	StartMS int    `json:"start_ms,omitempty"`
	EndMS   int    `json:"end_ms,omitempty"`
}

// SummaryCoverage is the parent→child linkage of a summary representation (SPEC
// §5.2): the SINGLE source representation whose chunks the summary summarizes,
// plus the range within it. A summary covers exactly one representation, never a
// mix, and that representation MUST belong to the summary's own document (the
// same-document invariant) — expansion therefore never leaves the document.
type SummaryCoverage struct {
	SourceRepID int64                `json:"source_rep_id"`
	Range       SummaryCoverageRange `json:"range"`
}

// SummaryMeta is the meta_json shape persisted on a `summary` representation
// (SPEC §5.2). Provider/Model/PromptVersion/PromptHash form the generator side
// of the summary derivation identity (§8.6.7); Coverage is the parent→child
// linkage consumed by coarse-to-fine expansion (§9.7).
type SummaryMeta struct {
	SummaryLevel  string          `json:"summary_level"`
	Provider      string          `json:"provider,omitempty"`
	Model         string          `json:"model,omitempty"`
	ModelVersion  string          `json:"model_version,omitempty"`
	PromptVersion string          `json:"prompt_version,omitempty"`
	PromptHash    string          `json:"prompt_hash,omitempty"`
	Language      string          `json:"language,omitempty"`
	Coverage      SummaryCoverage `json:"coverage"`
}

// Valid reports whether the coverage linkage is structurally usable for
// expansion (SPEC §5.2): a positive source rep id and a known range kind whose
// bounds satisfy start <= end. An invalid coverage is treated as "no summary" —
// expansion skips it and retrieval falls back to the flat path for that unit.
func (c SummaryCoverage) Valid() bool {
	if c.SourceRepID <= 0 {
		return false
	}
	switch c.Range.Kind {
	case SummaryRangeDocument:
		return true
	case SummaryRangeOrdinals:
		return c.Range.Start >= 0 && c.Range.Start <= c.Range.End
	case SummaryRangeTime:
		return c.Range.StartMS >= 0 && c.Range.StartMS <= c.Range.EndMS
	default:
		return false
	}
}

// SummaryTimeRangeSelects reports whether a transcript segment / clip spanning
// [segStartMS, segEndMS] is selected by an inclusive summary time range
// [startMS, endMS]. Selection is by INTERVAL OVERLAP, not containment (SPEC
// §5.2): a segment that straddles a window endpoint is evidence the summary was
// built from and must not be dropped by coarse-to-fine expansion.
func SummaryTimeRangeSelects(startMS, endMS, segStartMS, segEndMS int) bool {
	return segStartMS <= endMS && segEndMS >= startMS
}

type Chunk struct {
	ChunkID         uint64
	RepID           int64
	Ordinal         int
	Text            string
	TextHash        string
	IndexKind       string
	EmbeddingStatus string
	EmbeddingError  string
	// ErrorCategory is the coarse failure classification persisted with a
	// chunk when it is inserted already-failed (e.g. quarantined by the
	// output quality gate). It mirrors store.ErrorCategory but is typed as a
	// plain string here to avoid an import cycle. Empty for healthy chunks.
	ErrorCategory string
	Deleted       bool
	// Modality is the chunk's content modality for multimodal embeddings
	// (SPEC 8.1.7): "" / "text" (default), or "image"/"audio"/"video"/"pdf".
	Modality string
	// MediaRef is the corpus rel_path of the source media for a non-text
	// chunk; the embedding worker reads those bytes and embeds them
	// directly. Empty for text chunks.
	MediaRef string
	// Language is the effective BCP-47 language of the chunk's source
	// representation (SPEC §5.2/§8.8), denormalized onto the chunk so the
	// per-language retrieval filter (§9.5) can predicate at candidate selection
	// without re-reading representation meta_json. It is populated at chunk
	// insert time from the representation's recorded meta language; empty means
	// the representation recorded no language (unknown), which never matches a
	// specific filter. Additive: a corpus indexed before any language was
	// recorded simply has empty values here (no migration, §9.5).
	Language string
	// Context is the generated document-aware context for contextual retrieval
	// (SPEC §5.3 `chunk_context` / §8.1.8, issue #330). It is prepended to the
	// text sent to the EMBEDDER only — never to Text, which stays the raw,
	// displayed and CITED chunk (citation faithfulness, #403). Empty when
	// contextual retrieval is off or the chunk fell back to raw.
	Context string
	// EmbeddingMode is the per-chunk contextualization state (SPEC §5.3
	// `embedding_mode`): EmbeddingModeDisabled, EmbeddingModeContextualized, or
	// EmbeddingModeFallback. It disambiguates an empty Context — feature off vs.
	// context generated vs. generation failed — and is what the re-embed gate
	// reads to retry fallback chunks. It is NOT part of the embed identity
	// (§8.1.4): it is per-chunk state within one contextual corpus. Empty
	// normalizes to EmbeddingModeDisabled.
	EmbeddingMode string
}

// Per-chunk contextualization states (SPEC §5.3 `embedding_mode` / §8.1.8).
const (
	// EmbeddingModeDisabled means contextual retrieval was off for this chunk
	// (feature disabled, fell open to off, or the chunk has no text to
	// contextualize, e.g. a media chunk): it embedded raw with no context.
	EmbeddingModeDisabled = "disabled"
	// EmbeddingModeContextualized means a context was generated and prepended to
	// this chunk's embed input.
	EmbeddingModeContextualized = "contextualized"
	// EmbeddingModeFallback means context generation FAILED for this chunk, so it
	// embedded raw (fail-open per chunk). Such a chunk is retried on the next scan
	// while contextualization stays on, and is counted in honest coverage — never
	// a silent, permanent hole.
	EmbeddingModeFallback = "fallback"
)

// NormalizeEmbeddingMode maps an empty/unknown value to EmbeddingModeDisabled so
// a pre-feature row (no recorded mode) reads as "contextual retrieval was off",
// exactly as SPEC §5.3 requires.
func NormalizeEmbeddingMode(mode string) string {
	switch mode {
	case EmbeddingModeContextualized, EmbeddingModeFallback:
		return mode
	default:
		return EmbeddingModeDisabled
	}
}

type Span struct {
	Kind      string
	StartLine int
	EndLine   int
	Page      int
	StartMS   int
	EndMS     int
	// Region is set only when Kind == "region" (structured document
	// extraction, spec §5.4). It carries the page range, primary-page
	// bounding box, and section breadcrumb. nil for all other kinds, which
	// keeps Span comparable for the scalar kinds.
	Region *RegionSpan
	// Words optionally carries per-word timing for a "time" span when the STT
	// provider returned word-level timestamps (spec §8.6.1). It is metadata
	// only: it never changes the chunk text and never creates extra chunks.
	// Persisted in the span's extra_json as a `words` array. Nil/empty for
	// every other span kind and for providers without word timing, which keeps
	// behaviour identical to a words-absent transcript. Its presence makes Span
	// non-comparable, so callers must not use Span as a map key.
	Words []WordSpan
	// Speaker is the stable per-transcript speaker identifier (e.g. "S1") on a
	// diarized transcript's "time" span (spec §8.6.8). SpeakerLabel is an
	// optional human-readable name (e.g. a WebVTT <v Name> voice tag). Both are
	// metadata only and additive: they never change the chunk text or span
	// bounds, and they are empty on every non-diarized transcript so behaviour
	// is byte-identical to today. Persisted in the "time" span's extra_json.
	Speaker      string
	SpeakerLabel string
	// Entities and Event carry a recognition annotation's structured
	// attribution on its "time" span (dirstral-spec design 0004 §7): the
	// entity ids the annotation references, and the backend-declared event
	// string describing what the annotation is about.
	//
	// They are what makes an entity filter role-exact. The wire shape has no
	// per-entity role, so role lives in annotation granularity: a backend
	// emits one annotation per role and distinguishes them with Event, and
	// selecting on entity AND event recovers the role. Keeping the ids
	// without the event recovers only half the query.
	//
	// Metadata only, exactly like Speaker: never changes chunk text or span
	// bounds, empty for every representation that is not a recognition
	// annotation, so behaviour is byte-identical where absent. Persisted in
	// the "time" span's extra_json.
	Entities []string
	Event    string
}

// WordSpan is one per-word timestamp on a "time" span (spec §8.6.1). The JSON
// tags match the stored extra_json shape exactly: `t` = word start in ms, `d` =
// word duration in ms, `w` = the word/token text.
type WordSpan struct {
	T int    `json:"t"`
	D int    `json:"d"`
	W string `json:"w"`
}

// FormatTranscriptTimestamp renders an absolute media offset (milliseconds) as
// the leading per-segment marker that transcribers emit and the ingest chunker
// consumes. It is the single source of truth for that wire format so every STT
// backend stays byte-for-byte consistent (generic, provider-neutral).
//
// A whole-second offset renders as "[mm:ss]" (unchanged from the historical
// format); a sub-second offset renders as "[mm:ss.mmm]" so that two segments
// inside the same second do not collapse onto one marker and word/subtitle
// timings keep their millisecond precision (issue #431 item c). Minutes are not
// wrapped into an hours field, matching prior provider output; a negative offset
// is clamped to zero. The companion parser (ingest.parseTranscriptTimestamp)
// accepts both forms, treating a bare whole-second marker as ".000".
func FormatTranscriptTimestamp(startMS int) string {
	if startMS < 0 {
		startMS = 0
	}
	totalSeconds := startMS / 1000
	ms := startMS % 1000
	mm := totalSeconds / 60
	ss := totalSeconds % 60
	if ms == 0 {
		return fmt.Sprintf("[%02d:%02d]", mm, ss)
	}
	return fmt.Sprintf("[%02d:%02d.%03d]", mm, ss, ms)
}

// RegionSpan localizes a chunk to a rectangular area within a page range of a
// structured document (dirstral-spec §5.4 "region" span kind). It is the
// in-memory shape of the spans.extra_json blob for region spans.
type RegionSpan struct {
	StartPage int      `json:"-"`
	EndPage   int      `json:"-"`
	BBox      *BBox    `json:"bbox,omitempty"`
	Section   []string `json:"section,omitempty"`
	Label     string   `json:"label,omitempty"`
}

// BBox is a bounding box in the source document's point space. CoordOrigin is
// the origin actually stored ("TOPLEFT" or "BOTTOMLEFT").
type BBox struct {
	Page        int     `json:"page"`
	L           float64 `json:"l"`
	T           float64 `json:"t"`
	R           float64 `json:"r"`
	B           float64 `json:"b"`
	CoordOrigin string  `json:"coord_origin"`
}

// NormalizeCoordOrigin constrains a bbox coord_origin to the §5.4 enum
// {TOPLEFT, BOTTOMLEFT}. Empty defaults to TOPLEFT (the spec's SHOULD-normalize
// target) and any other unrecognized value is clamped to TOPLEFT, so an emitted
// span always satisfies the published Span schema's enum constraint.
func NormalizeCoordOrigin(origin string) string {
	if strings.EqualFold(strings.TrimSpace(origin), "BOTTOMLEFT") {
		return "BOTTOMLEFT"
	}
	return "TOPLEFT"
}

// NormalizeRegionLabel collapses a region span label to the §5.4 eight-value
// enum {paragraph, section_header, list_item, table, caption, code, formula,
// picture}. docling emits "title" (LabelTitle), which is outside the enum, so it
// collapses to section_header; any other non-empty unknown collapses to the
// neutral "paragraph". An empty label is left empty (the field is omitempty and
// optional), so a label-less span keeps its shape. Only the stored/emitted label
// is normalized — the internal LabelTitle used for document-title detection is
// untouched.
func NormalizeRegionLabel(label string) string {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return ""
	}
	switch strings.ToLower(trimmed) {
	case "paragraph", "section_header", "list_item", "table",
		"caption", "code", "formula", "picture":
		return strings.ToLower(trimmed)
	case "title":
		return "section_header"
	default:
		return "paragraph"
	}
}

// IndexPayload is the per-vector metadata an Index stores alongside the dense
// vector (issue #247). It carries everything retrieval needs to materialise a
// SearchHit and everything a Filter needs to predicate on, so an external or
// on-disk backend can serve filtered search without dir2mcp re-fetching chunk
// metadata from SQLite. Backends that cannot persist the full payload may store
// a subset; retrieval falls back to its in-memory chunk metadata when a field
// is empty.
type IndexPayload struct {
	ChunkID      uint64
	RelPath      string
	DocType      string
	RepType      string
	Modality     string
	Title        string
	StartMS      int
	EndMS        int
	Language     string
	Speaker      string
	SpeakerLabel string
	Snippet      string
	Span         Span
	MediaRef     string
}

// ToSearchHit materialises a SearchHit from the payload. Score is left zero;
// the caller sets it from the IndexHit.
func (p IndexPayload) ToSearchHit() SearchHit {
	span := p.Span
	// Some backends (e.g. Qdrant) store the diarized speaker as a flat payload
	// field but do not persist the nested Span (SPEC §8.6.8): bridge the flat
	// Speaker/SpeakerLabel onto a "time" span so the Go-side speaker filter and
	// the citation surface still see the attribution. The Span value is
	// authoritative when it already carries a speaker (e.g. pgvector round-trips
	// the full Span), so we only fill in the gap.
	if strings.EqualFold(strings.TrimSpace(span.Kind), "time") && strings.TrimSpace(span.Speaker) == "" {
		span.Speaker = p.Speaker
		span.SpeakerLabel = p.SpeakerLabel
	}
	return SearchHit{
		ChunkID:  p.ChunkID,
		RelPath:  p.RelPath,
		Title:    p.Title,
		DocType:  p.DocType,
		RepType:  p.RepType,
		Snippet:  p.Snippet,
		Span:     span,
		Modality: p.Modality,
		MediaRef: p.MediaRef,
		Language: p.Language,
	}
}

// IndexHit is one scored result from Index.Search. Score is the cosine
// similarity (higher is better). Payload carries the stored metadata for the
// matched vector.
type IndexHit struct {
	ChunkID uint64
	Score   float32
	Payload IndexPayload
}

// Filter expresses the predicates retrieval applies to candidate vectors. It is
// the in-process, backend-agnostic shape of dir2mcp's overfetch-then-filter
// logic (issue #247): a FilteringIndex that reports CanFilter true pushes these
// predicates down to the backend; otherwise retrieval evaluates Match in Go.
//
//   - PathPrefix: keep only rel_paths with this prefix, normalized via
//     NormalizePathPrefix and matched case-insensitively (ASCII) to agree with
//     the store's list_files LIKE query (issue #286).
//   - PathGlob:   keep only rel_paths matching this canonical path glob
//     (MatchGlob: segment-aware `*`, recursive `**`, ASCII case-insensitive —
//     the same matcher list_files uses, issue #441).
//   - DocTypes:   keep only these doc types (case-insensitive).
//   - ExcludeOrphans: drop chunks with an empty rel_path (orphaned/evicted).
//   - Speaker: keep only time-spanned transcript chunks attributed to this
//     stable speaker id (case-insensitive). Empty disables the predicate. A
//     corpus without diarized transcripts simply matches nothing (SPEC §8.6.8).
type Filter struct {
	PathPrefix     string
	PathGlob       string
	DocTypes       []string
	ExcludeOrphans bool
	Speaker        string
	// Languages restricts candidates to representations whose recorded effective
	// language (SPEC §5.2/§8.8) matches any requested BCP-47 tag (logical OR,
	// SPEC §9.5). Empty disables the predicate (no language filtering). A
	// candidate with no recorded language (unknown) never matches a non-empty
	// Languages filter.
	Languages []string
	// LanguageMatch selects the §9.5 matching mode for Languages:
	// LanguageMatchPrimary ("" default — case-insensitive primary-subtag match)
	// or LanguageMatchStrict (opt-in RFC 4647 Basic Filtering region/script
	// narrowing). Inert unless Languages is non-empty.
	LanguageMatch string
	// Entities / Events restrict candidates to recognition annotations
	// referencing any of the requested entity ids, and/or carrying any of the
	// requested event values (design 0004 §7). OR within each field, AND
	// across them. Empty disables the predicate.
	//
	// Unlike Speaker, which some backends carry as a flat payload field, the
	// attribution exists only on the nested Span. A backend that does not
	// persist the Span (Qdrant) therefore cannot evaluate this and declines
	// push-down, leaving it to the retrieval service's post-materialisation
	// re-check.
	Entities []string
	Events   []string
}

// IsZero reports whether the filter has no active predicate.
func (f Filter) IsZero() bool {
	return f.PathPrefix == "" &&
		f.PathGlob == "" &&
		len(f.DocTypes) == 0 &&
		!f.ExcludeOrphans &&
		strings.TrimSpace(f.Speaker) == "" &&
		len(f.Languages) == 0 &&
		len(f.Entities) == 0 &&
		len(f.Events) == 0
}

// Match reports whether the payload satisfies every active predicate. It
// reproduces the semantics of retrieval's matchFilters: an empty rel_path is
// rejected when ExcludeOrphans is set; PathPrefix is normalized and matched via
// MatchesPathPrefix (consistent with list_files); PathGlob uses the canonical
// MatchGlob (also shared with list_files, issue #441); DocTypes is a
// case-insensitive set membership.
func (f Filter) Match(p IndexPayload) bool {
	relPath := p.RelPath
	if f.ExcludeOrphans && strings.TrimSpace(relPath) == "" {
		return false
	}
	// Normalize path_prefix consistently with list_files / the store so search
	// pushed down to a FilteringIndex agrees with list_files (issue #286 Bug B).
	if !MatchesPathPrefix(relPath, f.PathPrefix) {
		return false
	}
	if f.PathGlob != "" {
		matched, err := MatchGlob(f.PathGlob, relPath)
		if err != nil || !matched {
			return false
		}
	}
	return f.matchesDocType(p) && f.matchesMetadata(p)
}

// matchesDocType is the case-insensitive doc_type set membership, split out of
// Match to keep that function within the repo's cyclomatic budget.
func (f Filter) matchesDocType(p IndexPayload) bool {
	if len(f.DocTypes) == 0 {
		return true
	}
	for _, dt := range f.DocTypes {
		if strings.EqualFold(strings.TrimSpace(dt), strings.TrimSpace(p.DocType)) {
			return true
		}
	}
	return false
}

// matchesMetadata evaluates the predicates that read a payload's recorded
// metadata rather than its path: the diarized speaker (SPEC §8.6.8), the
// recorded language (SPEC §9.5), and a recognition annotation's attribution
// (design 0004 §7).
//
// Each shares the same shape: a payload that records nothing for a predicate
// never matches a non-empty filter on it, so a corpus with no diarized
// transcripts (respectively no recorded language, no annotations) returns
// nothing rather than falling back to unfiltered results. An empty filter is a
// no-op in every case.
func (f Filter) matchesMetadata(p IndexPayload) bool {
	if speaker := strings.TrimSpace(f.Speaker); speaker != "" {
		if !strings.EqualFold(speaker, strings.TrimSpace(p.Speaker)) {
			return false
		}
	}
	if len(f.Languages) > 0 {
		if !LanguageMatchesAnyMode(p.Language, f.Languages, f.LanguageMatch) {
			return false
		}
	}
	return f.MatchesAnnotation(p.Span)
}

// MatchesAnnotation evaluates the recognition entity/event predicate against a
// span (design 0004 §7): OR within each field, AND across them, matched
// literally because ids and event strings are backend-declared tokens rather
// than prose. Exported so retrieval applies exactly this rule after
// materialisation, where the hit's span is in hand, rather than restating it.
func (f Filter) MatchesAnnotation(span Span) bool {
	if len(f.Entities) > 0 && !MatchesAnyLiteral(f.Entities, span.Entities) {
		return false
	}
	if len(f.Events) > 0 && !MatchesAnyLiteral(f.Events, []string{span.Event}) {
		return false
	}
	return true
}

// NormalizeEntityIDs trims, drops empties, and de-duplicates entity ids while
// preserving first-seen order. Order is preserved because a backend emits the
// acting entity first (design 0004 §8: role lives in annotation granularity).
//
// One rule shared by ingestion and persistence deliberately: the derivation
// hash covers what is STORED, so if the two normalized differently a backend
// that emitted a stray blank id would re-derive a representation whose stored
// attribution is byte-identical.
func NormalizeEntityIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MatchesAnyLiteral reports whether any requested value appears among the
// candidate's values, after trimming. Literal rather than case-insensitive:
// entity ids and event strings are opaque tokens declared by a backend, and
// folding case could collide two the backend considers distinct.
func MatchesAnyLiteral(requested, candidate []string) bool {
	if len(requested) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(candidate))
	for _, c := range candidate {
		if c = strings.TrimSpace(c); c != "" {
			have[c] = struct{}{}
		}
	}
	if len(have) == 0 {
		return false
	}
	for _, r := range requested {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if _, ok := have[r]; ok {
			return true
		}
	}
	return false
}

type SearchQuery struct {
	Query      string
	K          int
	Index      string
	PathPrefix string
	FileGlob   string
	DocTypes   []string
	// Speaker optionally restricts time-spanned transcript hits to segments
	// attributed to this stable speaker id (SPEC §8.6.8/§15.2). Empty disables
	// the filter; a corpus without diarized transcripts returns no
	// speaker-filtered hits.
	Speaker string
	// Languages optionally restricts hits to representations recorded in any of
	// these BCP-47 languages (SPEC §9.5/§15.2-3), logical OR. Absent/empty
	// disables the filter (unchanged behavior). An unknown-language
	// representation never matches a non-empty filter.
	Languages []string
	// LanguageMatch selects the §9.5 match mode for Languages: primary-subtag by
	// default ("" ⇒ LanguageMatchPrimary) or opt-in region/script narrowing
	// (LanguageMatchStrict). Inert unless Languages is non-empty.
	LanguageMatch string
	// Entities / Events optionally restrict hits to recognition annotations
	// (dirstral-spec design 0004 §7). Within a field the match is OR: a hit
	// matches if its annotation references ANY requested entity id
	// (respectively, if its event equals ANY requested value). Across the two
	// fields, and against every other filter, the match is AND — so
	// Entities=[team:x] with Events=[at_bat] is the role-exact selection that
	// entity ids alone cannot express.
	//
	// Values are matched literally. Event strings are backend-declared, so
	// this defines no vocabulary. Only annotation-derived hits carry these, so
	// a hit from any other representation never matches a non-empty filter,
	// mirroring how the media time-window filter admits only time-spanned
	// hits. Absent/empty disables the filter (unchanged behavior).
	Entities []string
	Events   []string
	// DateFrom / DateTo optionally restrict hits to a document-date window (SPEC
	// §9.6), compared against each candidate's source-document calendar anchor
	// (mtime_unix). Both are Unix seconds and both bounds are inclusive; 0 means
	// open on that side (absent bound). A candidate with an unknown mtime (0)
	// never matches a window that sets either bound. Both-zero disables the
	// filter (unchanged behavior). Callers validate DateFrom <= DateTo upstream.
	DateFrom int64
	DateTo   int64
	// TimeFromMS / TimeToMS optionally restrict hits to an intra-document media
	// time window (SPEC §9.8): non-negative millisecond offsets within a
	// document's timeline, both bounds inclusive, with overlap (not containment)
	// semantics. Each bound is meaningful only when its HasTimeFrom / HasTimeTo
	// flag is set — 0 is a valid lower bound (video start), so presence cannot be
	// inferred from the value, and a zero-value SearchQuery leaves the filter off.
	// The filter is active when EITHER bound is present; while active, only
	// time-spanned hits are eligible (a non-time span never matches), so a corpus
	// without time-spanned representations returns no time-filtered hits. Callers
	// validate TimeFromMS <= TimeToMS upstream.
	HasTimeFrom bool
	TimeFromMS  int
	HasTimeTo   bool
	TimeToMS    int
}

// RelatedQuery is the input to a query-by-example "more like this" retrieval
// (SPEC §15.12): exactly ONE of SourceChunkID / SourceRelPath identifies the
// seed segment (the tool layer enforces the oneOf), and the same §9.5/§9.6
// filters as SearchQuery narrow the returned neighbours.
type RelatedQuery struct {
	// SourceChunkID is the seed chunk when the request identified the source by
	// chunk_id; 0 when the source was given by rel_path.
	SourceChunkID uint64
	// SourceRelPath is the seed document (corpus-relative) when the request
	// identified the source by rel_path; "" when the source was given by chunk_id.
	SourceRelPath string
	K             int
	// Index selects the vector axis to search: auto|text|code|both. "auto"
	// matches the seed segment's own index_kind.
	Index string
	// ExcludeSameDocument widens seed exclusion for a chunk_id request: when true
	// (default) every chunk of the seed chunk's document is excluded; when false
	// only the seed chunk itself is excluded. No-op for a rel_path request — a
	// document's own chunks are always excluded.
	ExcludeSameDocument bool
	PathPrefix          string
	FileGlob            string
	DocTypes            []string
	Languages           []string
	LanguageMatch       string
	DateFrom            int64
	DateTo              int64
}

// RelatedResult is the output of a RelatedSearcher.Related call (SPEC §15.12).
type RelatedResult struct {
	// SourceChunkID / HasSourceChunkID echo the resolved seed chunk id for a
	// chunk_id request; HasSourceChunkID is false for a rel_path request so the
	// field is omitted from the tool output.
	SourceChunkID    uint64
	HasSourceChunkID bool
	// SourceRelPath is the resolved seed document rel_path (present for both
	// request shapes).
	SourceRelPath string
	K             int
	// IndexUsed is the axis actually searched (text|code|both).
	IndexUsed        string
	Hits             []SearchHit
	IndexingComplete bool
}

type SearchHit struct {
	ChunkID uint64
	RelPath string
	Title   string
	DocType string
	RepType string
	Score   float64
	Snippet string
	Span    Span
	// Modality / MediaRef identify a multimodal media chunk (SPEC 8.1.7):
	// Modality is image/audio/video/pdf for a media chunk (empty for text),
	// and MediaRef is the corpus rel_path whose bytes were embedded. They let
	// retrieval dedup page-image candidates and mark media-only hits.
	Modality string
	MediaRef string
	// Language is the effective BCP-47 language of the hit's source
	// representation (SPEC §5.2/§8.8), carried so the per-language retrieval
	// filter (§9.5) can predicate on candidates. It is internal to retrieval and
	// is NOT serialized into the tool result (the §9.2 hit structure is
	// unchanged); empty means unknown.
	Language string
	// MTimeUnix is the source document's calendar anchor (its modification time,
	// Unix seconds), denormalized onto the hit so the date/time-range retrieval
	// filter (SPEC §9.6) can predicate on candidates without a store lookup —
	// mirroring Language above. It is internal to retrieval and is NOT serialized
	// into the tool result (the §9.2 hit structure is unchanged); 0 means unknown.
	MTimeUnix int64
	// EvidenceScore / EvidenceScale carry the ABSOLUTE relevance signal for this
	// hit: a score whose meaning does not depend on the other hits in the same
	// response (SPEC §9.4.3). Score alone cannot serve that role because a
	// rank-based RRF fusion score encodes rank rather than relevance, and the
	// index=both path rescales each axis against its own best; both destroy the
	// absolute reading. EvidenceScale names the scale EvidenceScore is on, so the
	// insufficient-evidence threshold can be maintained per scale instead of one
	// number applied across incommensurable scales:
	//
	//	"cosine" - query/chunk cosine similarity from the vector index
	//	"rerank" - the reranker's own relevance score for the (query, chunk) pair
	//	""       - no absolute signal available (e.g. a BM25-only fused candidate)
	//
	// They are internal to retrieval and are NOT serialized into the tool result
	// (the §9.2 hit structure is unchanged), mirroring Language / MTimeUnix above.
	EvidenceScore float64
	EvidenceScale string
}

type ChunkMetadata struct {
	ChunkID  uint64
	RelPath  string
	Title    string
	DocType  string
	RepType  string
	Snippet  string
	Span     Span
	Modality string
	MediaRef string
	// Language is the effective BCP-47 language of the chunk's source
	// representation (SPEC §5.2/§8.8), denormalized from representation meta_json
	// at chunk insert so retrieval can apply the per-language filter (§9.5).
	Language string
	// MTimeUnix is the source document's calendar anchor (its modification time,
	// Unix seconds), carried alongside the chunk so ToSearchHit can surface it to
	// the date/time-range retrieval filter (SPEC §9.6). 0 means unknown.
	MTimeUnix int64
}

// ToSearchHit converts the lightweight chunk metadata back into a full
// SearchHit.  This is convenient when code (e.g. retrieval) still operates on
// SearchHit values but chunk tasks only need a subset of fields.
func (m ChunkMetadata) ToSearchHit() SearchHit {
	return SearchHit{
		ChunkID:   m.ChunkID,
		RelPath:   m.RelPath,
		Title:     m.Title,
		DocType:   m.DocType,
		RepType:   m.RepType,
		Snippet:   m.Snippet,
		Span:      m.Span,
		Modality:  m.Modality,
		MediaRef:  m.MediaRef,
		Language:  m.Language,
		MTimeUnix: m.MTimeUnix,
	}
}

// ChunkTask represents a pending unit of work that needs an embedding.
//
// Label corresponds to the chunk_id in the SQLite schema and is always a
// positive integer; the type was changed to uint64 for consistency with the
// ANN index which also uses unsigned labels. Metadata is a small subset of
// SearchHit information that is relevant when processing the task
// (the score field is omitted since it isn’t applicable).
//
// Historically the identifier lived only in the Label field; adding
// ChunkMetadata caused duplication and opened the door for the two values to
// diverge. Label is retained for API compatibility with the embedding
// pipeline (EmbeddingWorker, stores etc.) but callers should prefer
// Metadata.ChunkID when they only need an ID. The helper constructors and
// validation method below ensure the two fields remain in sync.

// ChunkTask is intentionally a struct so that callers outside the package may
// construct values in tests or mocks but NewChunkTask should be used by
// production code whenever possible.
type ChunkTask struct {
	Label     uint64
	Text      string
	IndexKind string
	Metadata  ChunkMetadata
	// Modality / MediaRef carry multimodal-chunk info (SPEC 8.1.7): for a
	// non-text chunk, Modality is image/audio/video/pdf and MediaRef is the
	// corpus rel_path whose bytes the worker embeds. Empty/"text" for text.
	Modality string
	MediaRef string
	// TextHash is the chunk's persisted content hash (SPEC §5.3). It is the
	// chunk's PAYLOAD identity: chunk_id survives an in-place re-ingest of the
	// same (rep_id, ordinal) while the text, the hash and the index_kind are all
	// rewritten, so the id alone does not say WHICH bytes a task names. The
	// distributed coordinator stamps it onto every job (SPEC §8.7.2) so a worker
	// can tell a job for the chunk's current form from one enqueued for a form
	// that has since been replaced. Empty when the producer did not supply one;
	// consumers MUST treat empty as "unknown", never as a mismatch.
	TextHash string
	// Context is the chunk's generated document-aware context (SPEC §8.1.8,
	// issue #330). It participates in EmbedInput ONLY: Text remains the raw
	// chunk that snippets, reranking, and citations use, so the generated
	// context can never leak into a quote (citation faithfulness, #403). Empty
	// unless contextual retrieval produced a context for this chunk.
	Context string
}

// contextualEmbedSeparator joins a generated context to its chunk in the embed
// input (SPEC §8.1.8: `context + "\n\n" + chunk`).
const contextualEmbedSeparator = "\n\n"

// EmbedInput is the text actually sent to the embedder for this task. It is the
// raw chunk Text unless contextual retrieval generated a Context for the chunk,
// in which case it is `context + "\n\n" + chunk` (SPEC §8.1.8). This is the ONLY
// place the two are joined: every display/citation path reads Text directly, so
// the generated context never reaches a snippet, an open_file result, or an
// answer quote (#403).
func (t ChunkTask) EmbedInput() string {
	if t.Context == "" {
		return t.Text
	}
	return t.Context + contextualEmbedSeparator + t.Text
}

// NewChunkTask returns a task with the supplied components. If the provided
// metadata already contains a ChunkID it must match the explicit label;
// otherwise the metadata ID is populated. The function panics if the two
// values conflict, which is suitable for use by store code and tests where
// a mismatch indicates a programmer error. Callers that prefer an error
// return can instead construct a value and call Validate.
func NewChunkTask(label uint64, text, indexKind string, meta ChunkMetadata) ChunkTask {
	if meta.ChunkID == 0 {
		meta.ChunkID = label
	} else if label != meta.ChunkID {
		panic(fmt.Sprintf("NewChunkTask: label %d != metadata.ChunkID %d", label, meta.ChunkID))
	}
	return ChunkTask{
		Label:     label,
		Text:      text,
		IndexKind: indexKind,
		Metadata:  meta,
	}
}

// Validate checks that Label and Metadata.ChunkID agree. It returns an error
// if they differ and nil otherwise.
func (t ChunkTask) Validate() error {
	if t.Label != t.Metadata.ChunkID {
		return fmt.Errorf("label %d does not match metadata.chunkID %d", t.Label, t.Metadata.ChunkID)
	}
	return nil
}

type Citation struct {
	ChunkID uint64
	RelPath string
	Title   string
	Span    Span
}

type AskResult struct {
	Question         string
	Answer           string
	Citations        []Citation
	Hits             []SearchHit
	IndexingComplete bool
}

type CorpusStats struct {
	DocCounts       map[string]int64 `json:"doc_counts"`
	TotalDocs       int64            `json:"total_docs"`
	Scanned         int64            `json:"scanned"`
	Indexed         int64            `json:"indexed"`
	Skipped         int64            `json:"skipped"`
	Deleted         int64            `json:"deleted"`
	Representations int64            `json:"representations"`
	ChunksTotal     int64            `json:"chunks_total"`
	EmbeddedOK      int64            `json:"embedded_ok"`
	// EmbeddedPending is the count of non-deleted chunks still awaiting
	// embedding (embedding_status='pending'). Surfaced alongside EmbeddedOK so
	// "chunks_total>0 but embedded_ok=0" is unambiguous: a large pending count
	// with errors=0 means the embed worker hasn't drained the queue (semantic
	// search is degraded to keyword-only until it does), vs. a real embed
	// failure which shows under Errors/FailureSummary.
	EmbeddedPending int64 `json:"embedded_pending"`
	Errors          int64 `json:"errors"`
	Unknown         int64 `json:"unknown"`
	// FailureSummary groups chunk-level embedding failures by
	// store.ErrorCategory ("rate_limit", "payload_too_large", etc.)
	// plus a small sample of representative {rel_path, message}
	// pairs. Omitted from JSON when no failures have been recorded
	// so existing consumers continue to see a flat shape on healthy
	// corpora.
	FailureSummary *FailureSummary `json:"failure_summary,omitempty"`
	// SkipSummary groups documents that were recorded as skipped (never
	// indexed) by their SkipReason ("archive", "secret_excluded",
	// "unsupported_format", …) plus a small sample of representative
	// {rel_path, reason} pairs. It is the honest-coverage surface (#414/#395):
	// "what wasn't indexed & why". Omitted from JSON when nothing was skipped
	// so healthy corpora keep a flat shape. Only durably-persisted skip rows
	// contribute here; discovery-time drops that persist no row (path-excludes)
	// are surfaced separately by the in-run reindex summary.
	SkipSummary *SkipSummary `json:"skip_summary,omitempty"`
}

// SkipSummary aggregates skipped (never-indexed) documents for honest-coverage
// reporting. Categories is keyed by the string form of model.SkipReason* (kept
// as plain map[string]int64 so the model package needs no store dependency).
type SkipSummary struct {
	Categories map[string]int64 `json:"categories"`
	Samples    []SkipSample     `json:"samples,omitempty"`
}

// SkipSample is one representative skipped document: enough to see which files
// a given reason applies to without dumping the whole documents table.
type SkipSample struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// FailureSummary aggregates chunk embedding errors for diagnostics.
// Categories is keyed by the string form of store.ErrorCategory (kept
// as plain map[string]int64 here so the model package does not depend
// on internal/store).
//
// It describes the chunks that are CURRENTLY in a failed state, not the
// failures observed during the run that produced the enclosing snapshot. The
// distinction used to be invisible and actively misleading: corpus.json stamps
// each write with a fresh `ts`, so failures persisted hours earlier by a
// different run read as if this run had just made (and lost) that many
// provider calls. LastFailureUnix carries the age of the newest of those
// failures so a reader can tell "stranded since yesterday" from "failing right
// now" (issue #783).
type FailureSummary struct {
	Categories map[string]int64 `json:"categories"`
	Samples    []FailureSample  `json:"samples,omitempty"`
	// LastFailureUnix is when the most recent still-failed chunk was recorded
	// (UTC unix seconds). Zero (omitted) means no failed chunk carries a
	// timestamp — a corpus whose failures predate the embedding_failed_unix
	// column — and must be read as "age unknown", never as 1970.
	LastFailureUnix int64 `json:"last_failure_unix,omitempty"`
}

// FailureSample is one representative failed chunk: enough information
// to start triage without dumping the entire chunks table.
type FailureSample struct {
	RelPath  string `json:"rel_path"`
	Category string `json:"category"`
	Message  string `json:"message"`
	// FailedUnix is when this chunk was recorded as failed (UTC unix seconds),
	// or 0 when it predates the timestamp column. Per-sample rather than
	// summary-only so a mixed corpus (old stranded failures plus a fresh one)
	// is readable instead of collapsing to a single newest-wins timestamp.
	FailedUnix int64 `json:"failed_unix,omitempty"`
}

// MarshalJSON ensures that a nil DocCounts map is encoded as an empty object
// rather than null. This protects clients that expect an object and simplifies
// callers by avoiding repeated nil-checking before marshaling.
//
// Both value and pointer receivers will use this method, so callers may pass
// either form to json.Marshal. Stats defines its own MarshalJSON below which
// takes precedence over the promoted method, so embedding does not interfere
// with the outer struct's metadata fields.
func (c CorpusStats) MarshalJSON() ([]byte, error) {
	// Use an alias to avoid infinite recursion when calling json.Marshal.
	type alias CorpusStats
	if c.DocCounts == nil {
		// make a non-nil map so encoding/json treats it as {} instead of null
		c.DocCounts = make(map[string]int64)
	}
	return json.Marshal(alias(c))
}

type Stats struct {
	// metadata fields are kept explicitly so that they remain at the top
	// level when encoded to JSON.
	Root            string `json:"root"`
	StateDir        string `json:"state_dir"`
	ProtocolVersion string `json:"protocol_version"`

	// embed corpus statistics so that the various lifecycle counters are
	// promoted.
	//
	// NOTE: the default behaviour of encoding/json would *not* magically
	// flatten the embedded fields when the embedded type defines its own
	// MarshalJSON method. In that case the encoder would call
	// CorpusStats.MarshalJSON and include only the encoded result of the
	// embedded struct, dropping the outer metadata fields. To retain a
	// flat representation we implement Stats.MarshalJSON (below) which
	// explicitly merges the metadata fields with the promoted CorpusStats
	// fields.  This custom encoder is what actually provides the flattened
	// JSON output, not the default encoder behavior.
	CorpusStats
}

// MarshalJSON ensures that the metadata fields (root, state_dir,
// protocol_version) are serialized along with the flattened corpus
// statistics. It also guards against a nil DocCounts map so callers needn't
// worry about preinitializing the map prior to encoding.
//
// We cannot rely on the generic alias trick here because the embedded
// CorpusStats type has its own MarshalJSON method.  An alias type would
// still carry that method, causing json.Marshal to invoke CorpusStats's
// encoder and drop the metadata fields.  Instead we build a temporary struct
// that mirrors the exported JSON representation without any method set.
func (s Stats) MarshalJSON() ([]byte, error) {
	// ensure a non-nil map for the usual reason
	if s.DocCounts == nil {
		s.DocCounts = make(map[string]int64)
	}
	// Use an alias to strip methods from CorpusStats, then anonymously embed it
	// so json encoding flattens all corpus fields automatically.  The alias is
	// exported (capitalized) to avoid any risk of encoding/json treating an
	// unexported anonymous field as ignored when reflecting on the temporary
	// struct.
	type CorpusStatsFields CorpusStats
	type plain struct {
		Root            string `json:"root"`
		StateDir        string `json:"state_dir"`
		ProtocolVersion string `json:"protocol_version"`
		CorpusStatsFields
	}
	a := plain{
		Root:              s.Root,
		StateDir:          s.StateDir,
		ProtocolVersion:   s.ProtocolVersion,
		CorpusStatsFields: CorpusStatsFields(s.CorpusStats),
	}
	return json.Marshal(a)
}
