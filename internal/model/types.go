package model

import (
	"encoding/json"
	"fmt"
	"path"
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
}

// WordSpan is one per-word timestamp on a "time" span (spec §8.6.1). The JSON
// tags match the stored extra_json shape exactly: `t` = word start in ms, `d` =
// word duration in ms, `w` = the word/token text.
type WordSpan struct {
	T int    `json:"t"`
	D int    `json:"d"`
	W string `json:"w"`
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
	ChunkID  uint64
	RelPath  string
	DocType  string
	RepType  string
	Modality string
	Title    string
	StartMS  int
	EndMS    int
	Language string
	Speaker  string
	Snippet  string
	Span     Span
	MediaRef string
}

// ToSearchHit materialises a SearchHit from the payload. Score is left zero;
// the caller sets it from the IndexHit.
func (p IndexPayload) ToSearchHit() SearchHit {
	return SearchHit{
		ChunkID:  p.ChunkID,
		RelPath:  p.RelPath,
		Title:    p.Title,
		DocType:  p.DocType,
		RepType:  p.RepType,
		Snippet:  p.Snippet,
		Span:     p.Span,
		Modality: p.Modality,
		MediaRef: p.MediaRef,
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
//   - PathGlob:   keep only rel_paths matching this path.Match glob.
//   - DocTypes:   keep only these doc types (case-insensitive).
//   - ExcludeOrphans: drop chunks with an empty rel_path (orphaned/evicted).
type Filter struct {
	PathPrefix     string
	PathGlob       string
	DocTypes       []string
	ExcludeOrphans bool
}

// IsZero reports whether the filter has no active predicate.
func (f Filter) IsZero() bool {
	return f.PathPrefix == "" &&
		f.PathGlob == "" &&
		len(f.DocTypes) == 0 &&
		!f.ExcludeOrphans
}

// Match reports whether the payload satisfies every active predicate. It
// reproduces the semantics of retrieval's matchFilters: an empty rel_path is
// rejected when ExcludeOrphans is set; PathPrefix is normalized and matched via
// MatchesPathPrefix (consistent with list_files); PathGlob uses path.Match;
// DocTypes is a case-insensitive set membership.
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
		matched, err := path.Match(f.PathGlob, relPath)
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
	return true
}

type SearchQuery struct {
	Query      string
	K          int
	Index      string
	PathPrefix string
	FileGlob   string
	DocTypes   []string
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
	Errors          int64            `json:"errors"`
	Unknown         int64            `json:"unknown"`
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
