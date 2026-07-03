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

// DocumentHash pairs a document's canonical rel_path with its content_hash
// (SPEC §7.6). Retrieval-time cross-file de-duplication (SPEC §9.2) uses these
// to group candidate hits whose source documents are byte-identical.
type DocumentHash struct {
	RelPath     string
	ContentHash string
}

// DocumentHashLister is an optional capability stores may implement so the
// retrieval service can map a hit's rel_path to its content_hash for
// retrieval-time cross-file de-duplication (SPEC §9.2). The retrieval service
// type-asserts against this interface, mirroring the LexicalSearcher
// optional-capability pattern; stores that do not implement it simply yield no
// dedup map and search behaves exactly as before (pass-through). Only
// non-deleted documents are returned.
type DocumentHashLister interface {
	ListDocumentHashes(ctx context.Context) ([]DocumentHash, error)
}

// CorpusLanguageLister is an optional capability stores may implement so the
// retrieval service can resolve the "auto" cross-lingual query-expansion target
// set (#325) to the corpus's detected languages (#267). It returns the distinct
// non-empty effective languages recorded across non-deleted chunks (SPEC
// §5.2/§8.8), as BCP-47 tags. The retrieval service type-asserts against this
// interface, mirroring DocumentHashLister/LexicalSearcher; stores that do not
// implement it simply yield no auto targets and cross-lingual expansion is a
// no-op unless an explicit target list is configured.
type CorpusLanguageLister interface {
	ListCorpusLanguages(ctx context.Context) ([]string, error)
}

// Index is the core vector-store contract every backend must satisfy (issue
// #247). The in-memory HNSW is the conforming default; external/on-disk
// backends (Qdrant #268, pgvector #269, on-disk #246) implement the same
// interface. Optional capabilities (Persistable, FilteringIndex) are layered on
// top and discovered via type assertion, mirroring the LexicalSearcher /
// MultimodalEmbedder pattern.
//
// Implementations should be safe for concurrent use; retrieval calls Search
// concurrently with the embedding worker's Upsert.
type Index interface {
	// Upsert stores (or replaces) the vector and its payload, keyed by
	// payload.ChunkID. An empty vector is an error.
	Upsert(ctx context.Context, vector []float32, payload IndexPayload) error
	// Delete removes the vectors (and payloads) for the given chunk IDs.
	// Unknown IDs are ignored.
	Delete(ctx context.Context, chunkIDs []uint64) error
	// Search returns the k best matches for vector, ordered best-first.
	// A non-zero filter restricts the result set; backends that report
	// CanFilter false simply ignore it (retrieval applies the filter itself).
	Search(ctx context.Context, vector []float32, k int, filter Filter) ([]IndexHit, error)
	// Identity returns the recorded corpus-lifetime embed identity (SPEC
	// 8.1.4), or "" when the index is fresh.
	Identity(ctx context.Context) (string, error)
	// Reset clears all vectors/payloads and records identity as the new
	// corpus-lifetime embed identity. Called when the embed identity changes.
	Reset(ctx context.Context, identity string) error
	Close() error
}

// Persistable is the optional capability for indexes that can durably
// snapshot/restore themselves to a path (issue #247). The in-memory HNSW and
// the future on-disk backend implement it; networked backends (Qdrant,
// pgvector) own their persistence and do not. PersistenceManager type-asserts
// against this interface and silently skips indexes that do not implement it.
type Persistable interface {
	Save(ctx context.Context, path string) error
	Load(ctx context.Context, path string) error
}

// FilteringIndex is the optional capability for indexes that can evaluate a
// Filter inside the backend (issue #247). When CanFilter reports true for a
// given filter, retrieval pushes the filter down to Search and trusts the
// backend-filtered results (skipping the overfetch-then-filter loop); otherwise
// it falls back to fetching a wider candidate pool and filtering in Go.
type FilteringIndex interface {
	CanFilter(filter Filter) bool
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

// IndexAxisResolver is an optional Retriever capability that reports which
// physical index (text|code|both) a query will actually be routed to. The MCP
// search tool uses it to populate a truthful index_used (SPEC §15.2) — in
// particular so an "auto" query that routes to the code index is reported as
// "code" rather than the requested-name default of "text". Retrievers that do
// not implement it fall back to a name-derived index_used.
type IndexAxisResolver interface {
	ResolveIndex(query SearchQuery) string
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

// TokenEmbedding is the token-level output of a long-context embedder for a
// single input string (issue #332, late chunking). Vectors[i] is the
// contextualized embedding of the token spanning runes [Offsets[i], Ends[i]) of
// the ORIGINAL input string. Offsets/Ends are rune offsets (not byte offsets) so
// they line up with the rune-based chunk spans the ingest chunkers produce
// (chunkTextByChars et al. operate on []rune). len(Vectors) == len(Offsets) ==
// len(Ends), every vector has the same provider dimension, and the tokens are in
// reading order. A provider that tokenizes on bytes MUST convert to rune offsets
// before returning so the contract is uniform.
type TokenEmbedding struct {
	// Vectors holds one contextualized embedding per token, in reading order.
	Vectors [][]float32
	// Offsets[i] is the inclusive start rune offset of token i in the input.
	Offsets []int
	// Ends[i] is the exclusive end rune offset of token i in the input.
	Ends []int
}

// TokenEmbedder is an OPTIONAL capability an Embedder MAY implement to expose
// token-level (a.k.a. long-context) embeddings (issue #332, Jina "late
// chunking"). When the configured embedder implements it AND ingest.late_chunking
// is enabled, the ingest pipeline embeds the whole document ONCE via
// EmbedDocumentTokens, then mean-pools the token vectors within each chunk's rune
// span to produce that chunk's embedding — so each chunk carries document
// context. The pipeline type-asserts the active Embedder against this interface,
// mirroring the MultimodalEmbedder / StructuredTranscriber optional-capability
// pattern; an embedder that does NOT implement it makes late chunking fall back
// to today's chunk-then-embed (see latechunk.Decide). No shipped provider
// (Mistral/OpenAI/Cohere/Gemini) returns token embeddings today — this interface
// is the seam a future self-hosted token-embedding backend (e.g. TEI/Infinity)
// plugs into.
//
// Vectors returned here MUST be comparable to those from Embed (same provider/
// model/dimension/vector space) so a late-chunked corpus and a query embedded via
// Embed share one space.
type TokenEmbedder interface {
	Embedder
	// EmbedDocumentTokens returns the per-token contextualized embeddings for
	// each input document, aligned 1:1 with inputs. role is EmbedDocument at
	// index time. An input that exceeds the model's context window is the
	// implementation's concern (it MAY window/error); callers treat an error as
	// "fall back to chunk-then-embed".
	EmbedDocumentTokens(ctx context.Context, model string, role EmbedRole, inputs []string) ([]TokenEmbedding, error)
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

// TimedWord is one per-word timestamp returned by a structured transcriber.
// StartMS/EndMS are absolute offsets from the start of the media (spec §8.6.1).
type TimedWord struct {
	Word    string
	StartMS int
	EndMS   int
}

// TranscriptResult is the structured output of a StructuredTranscriber: the
// SAME `[mm:ss] text` segment string a plain Transcriber returns (so the
// existing chunker is unaffected), PLUS optional per-word timing. Words is nil
// when the provider returned no word-level timestamps.
type TranscriptResult struct {
	// Text is the segment-formatted transcript identical to Transcriber.Transcribe.
	Text string
	// Words is the flat, time-ordered list of per-word timestamps across the whole
	// transcript. Empty/nil when unavailable.
	Words []TimedWord
}

// StructuredTranscriber is an OPTIONAL capability a Transcriber MAY implement to
// expose per-word timing alongside the segment text (spec §8.6.1, issue #252).
// The ingest pipeline type-asserts the configured transcriber against this; a
// transcriber that does not implement it simply yields no word timing and the
// pipeline behaves exactly as before. Implementations MUST return a Text value
// byte-for-byte equal to what Transcribe would return for the same input so the
// downstream segment chunker is unchanged.
type StructuredTranscriber interface {
	Transcriber
	TranscribeStructured(ctx context.Context, relPath string, data []byte) (TranscriptResult, error)
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
