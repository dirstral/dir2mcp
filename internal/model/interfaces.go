package model

import "context"

type Store interface {
	Init(ctx context.Context) error
	UpsertDocument(ctx context.Context, doc Document) error
	GetDocumentByPath(ctx context.Context, relPath string) (Document, error)
	ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]Document, int64, error)
	Close() error
}

// LexicalSearcher is an optional capability stores may implement to support
// BM25 lexical search alongside the vector index. The retrieval service
// type-asserts against this interface to enable hybrid retrieval; stores that
// do not implement it transparently fall back to vector-only search.
//
// The indexKind argument filters by chunk index_kind ("text" or "code"); pass
// an empty string to search across both. The returned hits are ordered best
// first; Score is the BM25 score (lower magnitude is better in raw BM25, but
// implementations may negate to keep "higher is better" semantics — callers
// must rely on rank order, not score sign).
type LexicalSearcher interface {
	SearchBM25(ctx context.Context, query string, k int, indexKind string) ([]SearchHit, error)
}

type Index interface {
	Add(label uint64, vector []float32) error
	Search(vector []float32, k int) ([]uint64, []float32, error)
	Save(path string) error
	Load(path string) error
	Close() error
}

type Retriever interface {
	Search(ctx context.Context, query SearchQuery) ([]SearchHit, error)
	Ask(ctx context.Context, question string, query SearchQuery) (AskResult, error)
	OpenFile(ctx context.Context, relPath string, span Span, maxChars int) (string, error)
	Stats(ctx context.Context) (Stats, error)

	// IndexingComplete returns true if the underlying index has finished
	// processing new documents.  Callers previously had to invoke Ask and read
	// the flag from the resulting AskResult; this accessor provides a lightweight
	// alternative.  Implementations may always return true if they cannot
	// determine the state.
	IndexingComplete(ctx context.Context) (bool, error)
}

type Ingestor interface {
	Run(ctx context.Context) error
	Reindex(ctx context.Context) error
}

// EmbedRole distinguishes corpus/index-time embeddings from search-time
// query embeddings. Asymmetric providers (e.g. Cohere `input_type`,
// Voyage) MUST map it to their mechanism; symmetric providers (OpenAI,
// Mistral) accept it and MAY ignore it — behavior MUST NOT differ for
// symmetric providers. The role is set by the call site, not by config,
// and does not affect the corpus-lifetime embed identity (SPEC 8.1.4 /
// 8.1.5).
type EmbedRole string

const (
	// EmbedDocument is used when embedding corpus content at index time.
	EmbedDocument EmbedRole = "document"
	// EmbedQuery is used when embedding a search query at query time.
	EmbedQuery EmbedRole = "query"
)

type Embedder interface {
	Embed(ctx context.Context, model string, role EmbedRole, inputs []string) ([][]float32, error)
}

// MediaInput is one non-text item to embed (SPEC 8.1.7): the media bytes
// plus their MIME type (e.g. "image/png", "audio/mp3", "application/pdf").
type MediaInput struct {
	MimeType string
	Data     []byte
}

// MultimodalEmbedder is an optional capability for embedders that can embed
// non-text media into the SAME vector space as text (SPEC 8.1.7). The
// embedding worker type-asserts the configured Embedder against this to
// embed media chunks; an embedder that does not implement it cannot serve a
// multimodal corpus (validation rejects that config upstream). Vectors MUST
// be comparable to those from Embed (same provider/model/dimension).
type MultimodalEmbedder interface {
	Embedder
	EmbedMedia(ctx context.Context, model string, role EmbedRole, items []MediaInput) ([][]float32, error)
}

type OCR interface {
	Extract(ctx context.Context, relPath string, data []byte) (string, error)
}

// DocumentExtractor converts rich or binary document content into markdown-like
// text suitable for downstream chunking/indexing.
type DocumentExtractor interface {
	Extract(ctx context.Context, relPath string, data []byte) (string, error)
}

type Transcriber interface {
	Transcribe(ctx context.Context, relPath string, data []byte) (string, error)
}

type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// Reranked is one rescored candidate: Index is the position in the
// documents slice passed to Rerank; RelevanceScore is the provider's
// relevance (higher = better). Results are returned best-first.
type Reranked struct {
	Index          int
	RelevanceScore float64
}

// Reranker re-scores documents against a query (e.g. a cross-encoder
// rerank API). Retrieval is fail-open: callers treat any error as
// "skip rerank, keep the pre-rerank order". An empty documents slice
// yields (nil, nil) with no provider call.
type Reranker interface {
	Rerank(ctx context.Context, model, query string, documents []string, topN int) ([]Reranked, error)
}

// RepresentationStore defines the subset of store operations used by the
// ingest package for handling representations and their chunks.  It is
// defined here in the model package to avoid cyclic dependencies between the
// ingest and store packages; both can import model without forming a cycle.
// The interface mirrors the one previously declared inside ingest/represent.go
// but is now exported so other packages (like store) can implement it.
type RepresentationStore interface {
	UpsertRepresentation(ctx context.Context, rep Representation) (int64, error)
	InsertChunkWithSpans(ctx context.Context, chunk Chunk, spans []Span) (int64, error)
	SoftDeleteChunksFromOrdinal(ctx context.Context, repID int64, fromOrdinal int) error
	WithTx(ctx context.Context, fn func(tx RepresentationStore) error) error
}
