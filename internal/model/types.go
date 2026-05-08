package model

import (
	"encoding/json"
	"fmt"
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
	Status      string
	Deleted     bool
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
	Deleted         bool
}

type Span struct {
	Kind      string
	StartLine int
	EndLine   int
	Page      int
	StartMS   int
	EndMS     int
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
}

type ChunkMetadata struct {
	ChunkID uint64
	RelPath string
	Title   string
	DocType string
	RepType string
	Snippet string
	Span    Span
}

// ToSearchHit converts the lightweight chunk metadata back into a full
// SearchHit.  This is convenient when code (e.g. retrieval) still operates on
// SearchHit values but chunk tasks only need a subset of fields.
func (m ChunkMetadata) ToSearchHit() SearchHit {
	return SearchHit{
		ChunkID: m.ChunkID,
		RelPath: m.RelPath,
		Title:   m.Title,
		DocType: m.DocType,
		RepType: m.RepType,
		Snippet: m.Snippet,
		Span:    m.Span,
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
