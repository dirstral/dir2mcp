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
}

type Representation struct {
	RepID       int64
	DocID       int64
	RepType     string
	RepHash     string
	MetaJSON    string
	CreatedUnix int64
	Deleted     bool
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
	// language (SPEC §5.2/§8.8) matches any requested BCP-47 tag on the
	// primary-subtag axis, case-insensitively (logical OR, SPEC §9.5). Empty
	// disables the predicate (no language filtering). A candidate with no
	// recorded language (unknown) never matches a non-empty Languages filter.
	Languages []string
}

// IsZero reports whether the filter has no active predicate.
func (f Filter) IsZero() bool {
	return f.PathPrefix == "" &&
		f.PathGlob == "" &&
		len(f.DocTypes) == 0 &&
		!f.ExcludeOrphans &&
		strings.TrimSpace(f.Speaker) == "" &&
		len(f.Languages) == 0
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
	if len(f.DocTypes) > 0 {
		match := false
		for _, dt := range f.DocTypes {
			if strings.EqualFold(strings.TrimSpace(dt), strings.TrimSpace(p.DocType)) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	// Speaker restricts to time-spanned transcript segments attributed to the
	// requested stable speaker id (SPEC §8.6.8). A payload that carries no
	// speaker (non-diarized transcript or any non-time chunk) never matches a
	// non-empty speaker filter, so a corpus without diarized transcripts returns
	// no speaker-filtered hits.
	if speaker := strings.TrimSpace(f.Speaker); speaker != "" {
		if !strings.EqualFold(speaker, strings.TrimSpace(p.Speaker)) {
			return false
		}
	}
	// Languages restricts to representations recorded in any of the requested
	// BCP-47 languages, matched on the primary subtag case-insensitively (SPEC
	// §9.5). A payload with no recorded language (unknown, §8.8) never matches a
	// non-empty filter, so a corpus indexed before any language was recorded
	// returns nothing for a specific language filter. Empty filter is a no-op.
	if len(f.Languages) > 0 {
		if !LanguageMatchesAny(p.Language, f.Languages) {
			return false
		}
	}
	return true
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
	// these BCP-47 languages (SPEC §9.5/§15.2-3): case-insensitive primary-subtag
	// match, logical OR. Absent/empty disables the filter (unchanged behavior).
	// An unknown-language representation never matches a non-empty filter.
	Languages []string
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
}

// ToSearchHit converts the lightweight chunk metadata back into a full
// SearchHit.  This is convenient when code (e.g. retrieval) still operates on
// SearchHit values but chunk tasks only need a subset of fields.
func (m ChunkMetadata) ToSearchHit() SearchHit {
	return SearchHit{
		ChunkID:  m.ChunkID,
		RelPath:  m.RelPath,
		Title:    m.Title,
		DocType:  m.DocType,
		RepType:  m.RepType,
		Snippet:  m.Snippet,
		Span:     m.Span,
		Modality: m.Modality,
		MediaRef: m.MediaRef,
		Language: m.Language,
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
}

// FailureSummary aggregates chunk embedding errors for diagnostics.
// Categories is keyed by the string form of store.ErrorCategory (kept
// as plain map[string]int64 here so the model package does not depend
// on internal/store).
type FailureSummary struct {
	Categories map[string]int64 `json:"categories"`
	Samples    []FailureSample  `json:"samples,omitempty"`
}

// FailureSample is one representative failed chunk: enough information
// to start triage without dumping the entire chunks table.
type FailureSample struct {
	RelPath  string `json:"rel_path"`
	Category string `json:"category"`
	Message  string `json:"message"`
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
