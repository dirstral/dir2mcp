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

type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
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
