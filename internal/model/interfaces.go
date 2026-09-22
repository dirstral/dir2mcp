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

// IndexUpsert is one (vector, payload) pair for a batched upsert. It carries
// exactly the two arguments Index.Upsert takes, so a batch is semantically a
// sequence of Upserts applied in slice order (last writer wins on a repeated
// ChunkID).
type IndexUpsert struct {
	Vector  []float32
	Payload IndexPayload
}

// BatchUpserter is the OPTIONAL capability for indexes that can apply many
// upserts as a single unit (issue #429 F8). It exists because a per-write
// durability barrier (the on-disk backend fsyncs every appended record) caps
// ingest at the fsync rate: one fsync per chunk. A backend that implements this
// pays the barrier once per batch instead.
//
// Callers type-assert against it (mirroring Persistable / FilteringIndex /
// LexicalSearcher) and fall back to a per-item Upsert loop when the backend does
// not implement it, so memory/Qdrant/pgvector are unaffected.
//
// Contract:
//   - Applying items in order MUST be equivalent to calling Upsert once per
//     item: same validation (empty vector / zero ChunkID are errors), same
//     last-writer-wins semantics.
//   - Durability is per BATCH, not per item: on return with a nil error every
//     item is as durable as an individual Upsert would have been, but a crash
//     mid-batch may lose the whole batch. Callers must therefore not record a
//     batch's items as persisted anywhere until BatchUpsert has returned nil.
//   - On a non-nil error the index MUST be left in a state where replaying the
//     same items through Upsert is safe, either by rolling the batch back or
//     by relying on the last-writer-wins idempotency of Upsert. Callers use that
//     replay to attribute the failure to a specific item.
type BatchUpserter interface {
	BatchUpsert(ctx context.Context, items []IndexUpsert) error
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

// AxisSearcher is an optional Retriever capability that runs a search and also
// reports the physical index axis (text|code|both) the query was ACTUALLY
// dispatched on. The MCP search tool uses it to populate a truthful index_used
// (SPEC §15.2) taken from the real dispatch, so the reported value can never
// diverge from the axis searched — including HyDE "replace" mode, where routing
// keys off the generated hypothetical document rather than the original query,
// and an "auto" query whose route depends on the query text. Retrievers that do
// not implement it fall back to a name-derived index_used.
type AxisSearcher interface {
	SearchWithAxis(ctx context.Context, query SearchQuery) ([]SearchHit, string, error)
}

// RelatedSearcher is an optional Retriever capability that performs
// query-by-example "more like this" retrieval (SPEC §15.12, dir2mcp #324):
// given a seed chunk or document it returns the nearest-neighbour segments over
// the SAME vector index, excluding the seed itself. It is additive — the MCP
// dir2mcp_related tool type-asserts the retriever against this interface,
// mirroring AxisSearcher; a retriever that does not implement it simply does not
// expose the tool. Ordering is pure vector similarity (the reranker does NOT
// apply), so no query text is involved.
type RelatedSearcher interface {
	Related(ctx context.Context, query RelatedQuery) (RelatedResult, error)
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
// to chunk-then-embed (see latechunk.Decide). The first shipped implementation
// is internal/tei (a self-hosted Hugging Face Text Embeddings Inference server
// on its native surface, SPEC 8.1.1/8.1.9); the hosted kinds (Mistral, OpenAI,
// Cohere, Gemini) return pooled vectors only and do not implement it.
//
// Vectors returned here MUST be comparable to those from Embed (same provider/
// model/dimension/vector space) so a late-chunked corpus and a query embedded via
// Embed share one space. Failure contract (SPEC 8.1.9 "Failure classification"):
// a transient failure (network, 429, 5xx) MUST surface as a retryable error, and
// the worker leaves the chunks pending; a NON-retryable failure is a terminal
// failure of every chunk of that representation, recorded with its category and
// reason, never a per-document fall back to chunk-then-embed. The only fall back
// is corpus-wide and decided before any document is embedded: an embedder that
// does not implement this interface, or whose TokenEmbeddingProbe says the served
// model cannot provide comparable token embeddings.
type TokenEmbedder interface {
	Embedder
	// EmbedDocumentTokens returns the per-token contextualized embeddings for
	// each input document, aligned 1:1 with inputs. role is EmbedDocument at
	// index time. An input that exceeds the model's context window MUST be
	// windowed by the implementation (SPEC 8.1.9 "Long documents"); an error is
	// classified by the caller as transient (chunks stay pending) or terminal
	// (the representation's chunks are marked failed).
	EmbedDocumentTokens(ctx context.Context, model string, role EmbedRole, inputs []string) ([]TokenEmbedding, error)
}

// TokenEmbeddingProbe is an OPTIONAL capability of a TokenEmbedder: it answers,
// once per run and before any document is embedded, whether the SERVED model can
// provide token embeddings that share a space with Embed's pooled vectors (SPEC
// 8.1.9: a tei server must pool with `mean`). available=false with a reason is a
// definitive refusal, and the worker takes the corpus-wide fall back to
// chunk-then-embed with that reason logged once, exactly as for an embedder that
// lacks the interface. A non-nil err means the answer is unknown (the server was
// unreachable); the worker then keeps late chunking active and lets the
// per-document transient failures leave chunks pending, because a transient
// probe failure must not flip a pooled corpus to unpooled vectors.
type TokenEmbeddingProbe interface {
	TokenEmbeddingsAvailable(ctx context.Context) (available bool, reason string, err error)
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
	// Language is the source language the provider identified for THIS request,
	// as a BCP-47 primary subtag ("ru", "uk"), and LanguageConfidence the
	// provider's confidence in it in [0,1] (SPEC §8.2.2). Both are optional:
	// empty and 0 mean the provider reported nothing, and the pipeline falls back
	// to text detection or to the item's resolved language. A provider that
	// reports a language but no confidence leaves LanguageConfidence at 0; the
	// caller treats 0 as "unknown confidence", not as "no confidence".
	Language           string
	LanguageConfidence float64
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

// PayloadLimitedTranscriber is an OPTIONAL capability a Transcriber MAY implement
// to declare the largest audio payload it accepts in ONE request (issue #954).
// The ingest pipeline reads it before transcribing: a recording whose extracted
// audio exceeds the cap is decoded in windows instead of being sent whole and
// refused outright, which used to stamp the document status=error with no
// transcript at all.
//
// A transcriber that does not implement it counts as uncapped, and is windowed on
// the duration rule alone. Implementations MUST report the SAME limit they
// enforce, in bytes; 0 (or negative) means "no declared cap".
type PayloadLimitedTranscriber interface {
	Transcriber
	MaxTranscribePayloadBytes() int
}

// AudioDurationTranscriber is an OPTIONAL capability a Transcriber MAY implement
// to size ONE request to the audio that request carries (issue #962). The ingest
// pipeline knows the duration of every slice it sends; the transcriber knows what
// its own request timeout costs. This interface joins the two.
//
// ForAudioDuration returns the transcriber to use for a request carrying audioMS
// of audio. Implementations MUST NOT mutate the receiver (the same client serves
// concurrent documents): they return either the receiver itself or a copy.
// Returning the receiver is always valid, and is what an implementation does when
// the operator configured an explicit timeout, because an explicit setting wins
// over a derived one.
type AudioDurationTranscriber interface {
	Transcriber
	ForAudioDuration(audioMS int) Transcriber
}

// TranscriberForAudioDuration returns the transcriber to use for one request
// carrying audioMS of audio. A transcriber without the optional capability, or an
// unknown duration (audioMS <= 0), is returned unchanged, so every caller can size
// its request without first testing for the capability.
func TranscriberForAudioDuration(stt Transcriber, audioMS int) Transcriber {
	sized, ok := stt.(AudioDurationTranscriber)
	if !ok || audioMS <= 0 {
		return stt
	}
	if out := sized.ForAudioDuration(audioMS); out != nil {
		return out
	}
	return stt
}

// RecognizedAnnotation is one time-ranged statement a recognition backend
// makes about a media file's content (design 0004 §5). StartMS/EndMS are
// absolute offsets from the start of the media.
type RecognizedAnnotation struct {
	StartMS    int
	EndMS      int
	Event      string
	Entities   []string
	Text       string
	Confidence float64
	Sources    []string
	// Attributes are producer-defined key/value scopes (SPEC §9.10, design
	// 0006). Keys with the reserved dir2mcp: prefix are a contract violation
	// the producer MUST NOT emit; ingestion drops such an annotation as
	// malformed while its siblings proceed (design 0004 §5).
	Attributes map[string]string
}

// RecognizeResult is a recognition backend's full response for one media
// file. Name/Version identify the backend and feed the persisted
// representation's derivation identity (design 0004 §4).
type RecognizeResult struct {
	Name        string
	Version     string
	Annotations []RecognizedAnnotation
}

// Recognizer runs content recognition over a media file (design 0004): given
// the absolute path of a local media file, it returns time-ranged annotation
// statements. Recognizers receive a path rather than bytes because media
// files are large and served backends read them directly from disk.
type Recognizer interface {
	Recognize(ctx context.Context, absPath string) (RecognizeResult, error)
}

type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// BoundedGenerator is an OPTIONAL capability a Generator MAY implement to cap a
// single completion's output tokens for THIS call, without changing the
// generous default the generator uses for unbounded callers (answer synthesis,
// annotation). A caller with a known-short output (e.g. one translated
// transcript line) type-asserts its Generator against this and, if satisfied,
// passes a tight cap; a Generator that does not implement it falls back to
// Generate and behaves exactly as before. A maxTokens <= 0 MUST behave like
// Generate (use the generator's own default).
type BoundedGenerator interface {
	Generator
	GenerateWithMaxTokens(ctx context.Context, prompt string, maxTokens int) (string, error)
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

// RepresentationTextStore is an OPTIONAL capability of a RepresentationStore:
// it persists a representation's whole document text (SPEC §5.2
// `representation_texts`, §8.1.9), the string every chunk's rune span
// (Chunk.RuneStart/RuneEnd) indexes into. Ingest type-asserts the store (and
// the transaction handle WithTx supplies) against it and writes the text only
// while late chunking is enabled, so a corpus with the mode off carries no
// duplicate of its text. A store that lacks the capability cannot serve the
// late-chunking pooling path; the embedding worker reports that loudly rather
// than embedding chunk-then-embed under an identity that says otherwise.
type RepresentationTextStore interface {
	UpsertRepresentationText(ctx context.Context, repID int64, text string) error
	// DeleteRepresentationText removes the persisted text of a representation.
	// Ingest calls it when it rewrites a representation's chunks with late
	// chunking OFF: rep_id is stable per (document, rep_type), so a text left
	// from an earlier late-chunking run would otherwise be paired with the new
	// chunks' spans by a later late-chunking run and pool the wrong runes
	// without any error. Deleting a text that does not exist is not an error.
	DeleteRepresentationText(ctx context.Context, repID int64) error
}

// RepresentationTextReader is the read half of the late-chunking inputs
// (SPEC §8.1.9): the embedding worker type-asserts its chunk source against it
// to fetch the document text of every representation in a batch. ok is false
// when no text was persisted for repID (a representation written with the mode
// off, or before the table existed), which the worker treats as a per-chunk
// error with a reindex remediation, never as a reason to embed chunk-then-embed.
type RepresentationTextReader interface {
	RepresentationText(ctx context.Context, repID int64) (text string, ok bool, err error)
}
