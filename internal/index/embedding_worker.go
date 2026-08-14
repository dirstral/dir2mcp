package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/latechunk"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/pdfutil"
	"github.com/dirstral/dir2mcp/internal/store"
)

type ChunkSource interface {
	NextPending(ctx context.Context, limit int, indexKind string) ([]model.ChunkTask, error)
	MarkEmbedded(ctx context.Context, labels []uint64) error
	MarkFailed(ctx context.Context, labels []uint64, reason string) error
	// MarkFailedWithCategory is the classification-aware variant. New
	// failure write sites should use it so dir2mcp status / doctor /
	// support-bundle can group errors by cause. category is the string
	// form of store.ErrorCategory so the interface contract stays
	// loose — implementations don't need to depend on the store
	// package's type.
	MarkFailedWithCategory(ctx context.Context, labels []uint64, category, reason string) error
}

type EmbeddingWorker struct {
	Source         ChunkSource
	Index          model.Index
	Embedder       model.Embedder
	ModelForText   string
	ModelForCode   string
	BatchSize      int
	OnIndexedChunk func(label uint64, metadata model.ChunkMetadata)

	// MaxInputRunes caps the rune length of a single TEXT embed input. A chunk
	// longer than this is skipped up-front — marked embedding_failure with a
	// clear reason (skipOversizeText) — instead of being sent to the provider,
	// where an over-token input 400s the whole HTTP batch and, absent per-item
	// isolation, poisons its healthy siblings (issue #399 items 1+2). Pre-skipping
	// also avoids the wasted round-trip + bisection that isolation would otherwise
	// cost. 0 selects defaultMaxEmbedInputRunes. Media chunks (embedded from bytes,
	// not text) are exempt; their size is governed by the provider's byte ceiling.
	MaxInputRunes int

	// LateChunking is the resolved value of config.IngestLateChunking (issue
	// #332). It records the operator's intent to use the late-chunking path
	// (embed whole documents, mean-pool token vectors per chunk span) when the
	// configured Embedder implements model.TokenEmbedder.
	//
	// NOT-YET-WIRED (issue #446): the production embed loop does not yet run the
	// pooling path — the whole-doc-embed + per-chunk mean-pool needs per-document
	// rune spans and the source document text, neither of which the per-chunk
	// ChunkTasks the worker leases from NextPending carry today. Until that
	// plumbing lands the worker embeds chunk-by-chunk regardless of this flag, so
	// the one-time decision log (logLateChunkDecisionOnce) says so honestly and
	// never claims the path is "active". Default false: the worker behaves
	// byte-for-byte as before. The pure embed/mean-pool step already lives in
	// internal/latechunk; the worker holds the resolved capability gate
	// (latechunk.Decide) so the decision and any fallback are observable
	// end-to-end.
	LateChunking bool

	// lateChunkLogged guards the one-time late-chunking decision log so the
	// message is emitted once per worker, not on every RunOnce cycle.
	lateChunkLogged bool

	// RootDir is the corpus root used to resolve a media chunk's MediaRef
	// (a corpus rel_path) to bytes for multimodal embedding (SPEC 8.1.7).
	// Required only when media chunks are present; text-only corpora may
	// leave it empty.
	RootDir string

	// Corpus is the filesystem abstraction used to read a media chunk's bytes.
	// Nil means "use a local filesystem rooted at RootDir" — the default that
	// preserves the historical local-corpus behavior. Resolved lazily via
	// corpusFS() so callers never observe a nil backend.
	Corpus corpusfs.CorpusFS

	// MaxFileBytes is the resolved `ingest.max_file_mb` cap in bytes, supplied by
	// the caller from the single resolver (ingest.ResolvedMaxFileBytes) so this
	// worker enforces the SAME number discovery and the ingest source reads do
	// (#830). It bounds the whole-file media read (image and PDF bytes), which is
	// held in memory for the embed call.
	//
	// Zero or negative selects corpusfs.DefaultMaxFileSizeBytes, so a caller that
	// forgets to set it gets the default bound rather than no bound at all. There
	// is deliberately no unlimited setting: a 29 GB file that reached a corpus is
	// what OOM-killed a live daemon (#682).
	MaxFileBytes int64

	// Logger is optional; if non‑nil its Printf method will be used for
	// informational messages. When nil the standard library's log package
	// is used. Logging is only performed for transient/retryable errors or
	// when a fatal condition occurs in Run().
	Logger *log.Logger

	// ErrCh is an optional channel that will receive fatal errors before
	// Run returns. The caller may provide a buffered channel if it wants to
	// monitor errors asynchronously; Run will still return the error as its
	// return value. The channel is never closed by EmbeddingWorker.
	ErrCh chan error

	// RunOnceFunc, if non‑nil, is invoked by Run instead of the receiver's
	// own RunOnce method. This hook exists primarily for testing and
	// allows callers that embed EmbeddingWorker to override the behaviour
	// without having to duplicate the entire Run implementation.
	//
	// Production code should rarely set this field.
	RunOnceFunc func(ctx context.Context, indexKind string) (int, error)

	// ExtractSegmentFunc overrides audio/video time-window extraction from a
	// local filesystem path (SPEC 8.1.7). Defaults to avutil.ExtractSegment
	// (ffmpeg) when nil; tests set it to avoid requiring the ffmpeg binary.
	// Production code should rarely set this field.
	ExtractSegmentFunc func(ctx context.Context, path string, startMS, endMS int) ([]byte, error)

	// ExtractSegmentURLFunc overrides audio/video time-window extraction from an
	// http(s) URL (issue #243): when the corpus backend can presign a
	// range-seekable URL, the worker cuts the window over HTTP so ffmpeg pulls
	// only the needed bytes instead of forcing a whole-object download. Defaults
	// to avutil.ExtractSegmentURL when nil; tests set it to avoid requiring the
	// ffmpeg binary or network. Production code should rarely set this field.
	ExtractSegmentURLFunc func(ctx context.Context, url, srcExt string, startMS, endMS int) ([]byte, error)
}

// validate checks that the worker is properly configured before use.
func (w *EmbeddingWorker) validate() error {
	if w.Source == nil || w.Index == nil || w.Embedder == nil {
		return errors.New("source, index, and embedder are required")
	}
	return nil
}

// validateChunkTasks checks that every task in the slice passes Validate().
func validateChunkTasks(tasks []model.ChunkTask) error {
	for _, t := range tasks {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("%w: invalid chunk task: %v", ErrFatal, err)
		}
	}
	return nil
}

// buildEmbedBatch converts raw tasks into the parallel slices consumed by
// Embed.  Returns an ErrFatal-wrapped error when any task has a zero ChunkID.
func buildEmbedBatch(tasks []model.ChunkTask) (validTasks []model.ChunkTask, inputs []string, labels []uint64, err error) {
	validTasks = make([]model.ChunkTask, 0, len(tasks))
	inputs = make([]string, 0, len(tasks))
	labels = make([]uint64, 0, len(tasks))
	for _, task := range tasks {
		chunkID := task.Metadata.ChunkID
		if chunkID == 0 {
			return nil, nil, nil, fmt.Errorf("%w: zero label not supported", ErrFatal)
		}
		validTasks = append(validTasks, task)
		inputs = append(inputs, task.EmbedInput())
		labels = append(labels, chunkID)
	}
	return validTasks, inputs, labels, nil
}

// embedTasks embeds validTasks and returns vectors aligned 1:1 with them:
// text chunks go through Embedder.Embed, media chunks (SPEC 8.1.7) through
// MultimodalEmbedder.EmbedMedia after their bytes are read from RootDir.
// Both kinds share one model + vector space.
func (w *EmbeddingWorker) embedTasks(ctx context.Context, modelName string, validTasks []model.ChunkTask) ([][]float32, error) {
	vectors := make([][]float32, len(validTasks))

	// cache is scoped to this single embedTasks invocation so sibling chunks of
	// the same MediaRef (many PDF pages, many video time-windows) share one
	// backend fetch instead of each re-reading. It holds whole-file bytes and
	// materialized local paths only for the duration of the batch, and its
	// Localize temp files are cleaned up here at batch completion (issue #279).
	cache := newMediaBatchCache()
	defer cache.cleanup()

	textIdx := make([]int, 0, len(validTasks))
	textInputs := make([]string, 0, len(validTasks))
	mediaIdx := make([]int, 0)
	mediaItems := make([]model.MediaInput, 0)
	for i, t := range validTasks {
		if isMediaModality(t.Modality) {
			item, err := w.loadMediaInput(ctx, t, cache)
			if err != nil {
				return nil, err
			}
			mediaIdx = append(mediaIdx, i)
			mediaItems = append(mediaItems, item)
			continue
		}
		textIdx = append(textIdx, i)
		// EmbedInput, not Text: under contextual retrieval (SPEC §8.1.8) the
		// embedder receives `context + "\n\n" + chunk` while the chunk's stored,
		// displayed and CITED text stays raw (#403). With the feature off (the
		// default) EmbedInput IS Text, byte-for-byte.
		textInputs = append(textInputs, t.EmbedInput())
	}

	if len(textInputs) > 0 {
		v, err := w.Embedder.Embed(ctx, modelName, model.EmbedDocument, textInputs)
		if err != nil {
			return nil, err
		}
		if len(v) != len(textInputs) {
			return nil, fmt.Errorf("%w: embedding vector count mismatch", ErrFatal)
		}
		for k, idx := range textIdx {
			vectors[idx] = v[k]
		}
	}

	if len(mediaItems) > 0 {
		me, ok := w.Embedder.(model.MultimodalEmbedder)
		if !ok {
			return nil, fmt.Errorf("%w: embedder %T does not support media (multimodal) embedding", ErrFatal, w.Embedder)
		}
		v, err := me.EmbedMedia(ctx, modelName, model.EmbedDocument, mediaItems)
		if err != nil {
			return nil, err
		}
		if len(v) != len(mediaItems) {
			return nil, fmt.Errorf("%w: embedding vector count mismatch", ErrFatal)
		}
		for k, idx := range mediaIdx {
			vectors[idx] = v[k]
		}
	}
	// Last gate before the index (issue #703). Every shipped adapter already
	// validates its own response, but the worker accepts ANY model.Embedder —
	// including a distributed/self-hosted one this process does not own — and an
	// empty, non-finite or zero-norm vector is unretrievable-but-indistinguishable
	// once written. The error is non-transient and non-fatal, so embedIndexBatch
	// bisects the batch: the offending chunk is marked embedding_status=error
	// with this reason while its healthy siblings still embed (#399).
	if err := model.ValidateEmbedVectors("EMBED_OUTPUT_INVALID", 0, vectors); err != nil {
		return nil, err
	}
	return vectors, nil
}

// corpusFS resolves the active corpus filesystem, defaulting to a local
// filesystem rooted at RootDir when none was injected. This keeps local corpora
// behaving exactly as before while allowing an S3 (or other) backend.
func (w *EmbeddingWorker) corpusFS() corpusfs.CorpusFS {
	if w.Corpus != nil {
		return w.Corpus
	}
	return corpusfs.NewLocalFS(w.RootDir)
}

// loadMediaInput resolves a media chunk's source bytes (from the corpus
// filesystem + MediaRef) and infers its MIME type. PDFs are reduced to the
// chunk's single page and audio/video to the chunk's single time window, so the
// embedded bytes line up with the cited span and the per-request caps are
// respected (SPEC 8.1.7). The corpus filesystem constrains the ref to the
// corpus root as defense-in-depth against a traversal in a stored ref.
//
// Image and PDF bytes are read whole via Open+io.ReadAll (pdfutil needs the full
// file to extract a single page; see loadPDFPage for why a range-read ReadSeeker
// path is counterproductive with pdfcpu). Audio/video range-read on S3 when the
// backend can presign a URL — ffmpeg byte-range-seeks the window over HTTP — and
// otherwise fall back to Localize (a whole-object download on S3, a no-op locally)
// because ffmpeg cannot read from an io.Reader (see loadMediaSegment).
//
// All fetch paths go through cache (issue #279), so sibling chunks of the same
// MediaRef in one batch (every page of a PDF, every time-window of a video)
// share a single whole-file read, single Localize download, or single presigned
// URL instead of re-fetching per chunk. For the default LocalFS this is
// behavior-preserving (a local file is cheap to re-open and Localize is a no-op);
// it only removes redundant range GETs / full-object downloads for remote
// backends such as S3.
func (w *EmbeddingWorker) loadMediaInput(ctx context.Context, t model.ChunkTask, cache *mediaBatchCache) (model.MediaInput, error) {
	ref := strings.TrimSpace(t.MediaRef)
	if ref == "" {
		return model.MediaInput{}, fmt.Errorf("%w: media chunk %d has no media_ref", ErrFatal, t.Metadata.ChunkID)
	}
	if w.Corpus == nil && strings.TrimSpace(w.RootDir) == "" {
		return model.MediaInput{}, fmt.Errorf("%w: media embedding requires a corpus root", ErrFatal)
	}
	fsys := w.corpusFS()

	switch strings.ToLower(strings.TrimSpace(t.Modality)) {
	case "audio", "video":
		data, aerr := w.loadMediaSegment(ctx, t, fsys, ref, cache)
		if aerr != nil {
			return model.MediaInput{}, fatalIfEscape(aerr)
		}
		return model.MediaInput{MimeType: mediaMIMEType(ref), Data: data}, nil
	case "pdf":
		data, perr := w.loadPDFPage(ctx, t, fsys, ref, cache)
		if perr != nil {
			return model.MediaInput{}, fatalIfEscape(perr)
		}
		return model.MediaInput{MimeType: mediaMIMEType(ref), Data: data}, nil
	default: // image and other whole-file media
		data, rerr := cache.readWholeMedia(ctx, fsys, ref, w.mediaReadCapBytes())
		if rerr != nil {
			return model.MediaInput{}, fatalIfEscape(fmt.Errorf("read media %q: %w", ref, rerr))
		}
		return model.MediaInput{MimeType: mediaMIMEType(ref), Data: data}, nil
	}
}

// mediaBatchCache memoizes media fetches across the sibling chunks of one
// embedTasks invocation so a MediaRef shared by many chunks (a PDF read whole
// for every page, or a video downloaded once per time-window) is fetched from
// the corpus filesystem exactly once per batch instead of once per chunk
// (issue #279). It is created and disposed inside a single embedTasks call, so
// the cached whole-file bytes and any Localize temp files live only for the
// batch's duration — there is no process-global state or unbounded growth.
//
// The zero value is not usable; construct one with newMediaBatchCache.
type mediaBatchCache struct {
	// wholeFile holds bytes read via Open+io.ReadAll, keyed by MediaRef. Used
	// for image and PDF chunks (pdfutil needs the full file to extract a page),
	// so every page of one PDF shares a single whole-file read.
	wholeFile map[string][]byte
	// localized holds the materialized local path of a Localize'd MediaRef so
	// every audio/video time-window of one source shares a single download.
	localized map[string]string
	// mediaURLs holds presigned range-seekable URLs (issue #243) keyed by
	// MediaRef so every audio/video time-window of one S3 source presigns once
	// per batch and ffmpeg reads each window over HTTP from the shared URL.
	mediaURLs map[string]string
	// cleanups are the Localize cleanup funcs to invoke at batch completion;
	// holding them here (rather than deferring per call) keeps the materialized
	// path alive for sibling chunks and removes temp files exactly once.
	cleanups []func()
}

// newMediaBatchCache returns an empty, ready-to-use per-batch media cache.
func newMediaBatchCache() *mediaBatchCache {
	return &mediaBatchCache{
		wholeFile: make(map[string][]byte),
		localized: make(map[string]string),
		mediaURLs: make(map[string]string),
	}
}

// cleanup invokes every tracked Localize cleanup func (removing downloaded temp
// files) and resets the cache. It is safe to call on a nil receiver and to call
// more than once. embedTasks defers it so it runs at batch completion.
func (c *mediaBatchCache) cleanup() {
	if c == nil {
		return
	}
	for _, fn := range c.cleanups {
		if fn != nil {
			fn()
		}
	}
	c.cleanups = nil
	c.wholeFile = nil
	c.localized = nil
	c.mediaURLs = nil
}

// readWholeMedia returns the entire object at ref, reading it through the corpus
// filesystem only on the first request for that ref within the batch and serving
// cached bytes to sibling chunks. A nil cache falls back to an uncached read so
// the helper is usable without a batch context.
//
// maxBytes bounds the read (#830). The cap is one value per worker, so it is not
// part of the cache key: a cached entry was admitted under the same cap.
func (c *mediaBatchCache) readWholeMedia(ctx context.Context, fsys corpusfs.CorpusFS, ref string, maxBytes int64) ([]byte, error) {
	if c == nil {
		return readWholeMedia(ctx, fsys, ref, maxBytes)
	}
	if data, ok := c.wholeFile[ref]; ok {
		return data, nil
	}
	data, err := readWholeMedia(ctx, fsys, ref, maxBytes)
	if err != nil {
		return nil, err
	}
	c.wholeFile[ref] = data
	return data, nil
}

// localize returns a real local path for ref, calling fsys.Localize only on the
// first request for that ref within the batch and reusing the materialized path
// for sibling chunks. The Localize cleanup is tracked on the cache and runs at
// batch completion, not per call, so the path stays valid for all siblings. A
// nil cache falls back to a per-call Localize+cleanup so the helper is usable
// without a batch context.
func (c *mediaBatchCache) localize(ctx context.Context, fsys corpusfs.CorpusFS, ref string) (string, func(), error) {
	if c == nil {
		return fsys.Localize(ctx, ref)
	}
	if path, ok := c.localized[ref]; ok {
		return path, func() {}, nil
	}
	path, cleanup, err := fsys.Localize(ctx, ref)
	if err != nil {
		return "", nil, err
	}
	c.localized[ref] = path
	c.cleanups = append(c.cleanups, cleanup)
	// cleanup is owned by the cache; hand the caller a no-op so a per-call defer
	// cannot remove a temp file still needed by sibling chunks.
	return path, func() {}, nil
}

// mediaURL returns a range-seekable URL for ref when the corpus backend supports
// the MediaURLProvider capability (issue #243), presigning only on the first
// request for that ref within the batch and reusing it for sibling time-windows.
// ok=false means the backend cannot produce a URL (e.g. LocalFS) and the caller
// must fall back to localize. A nil cache presigns per call (no memoization) so
// the helper is usable without a batch context.
func (c *mediaBatchCache) mediaURL(ctx context.Context, fsys corpusfs.CorpusFS, ref string) (string, bool, error) {
	provider, ok := fsys.(corpusfs.MediaURLProvider)
	if !ok {
		return "", false, nil
	}
	if c != nil {
		if url, hit := c.mediaURLs[ref]; hit {
			return url, true, nil
		}
	}
	url, ok, err := provider.MediaURL(ctx, ref)
	if err != nil || !ok {
		return "", ok, err
	}
	if c != nil {
		c.mediaURLs[ref] = url
	}
	return url, true, nil
}

// fatalIfEscape promotes a corpus-root traversal error to ErrFatal so the worker
// treats a stored ref that escapes the corpus root as a permanent, non-retryable
// failure (preserving the pre-CorpusFS contract where the escape check returned
// ErrFatal). Other errors pass through unchanged so genuinely transient read
// failures (e.g. a temporarily unreadable file) stay retryable.
func fatalIfEscape(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, corpusfs.ErrPathEscapesRoot) {
		return fmt.Errorf("%w: %v", ErrFatal, err)
	}
	return err
}

// mediaReadCapBytes is the resolved per-file cap this worker bounds its media
// reads with. It hands the caller-supplied MaxFileBytes to the shared resolver,
// so an unset field selects the default bound and an absurd one is clamped rather
// than allowed to overflow a limit+1 (#830).
func (w *EmbeddingWorker) mediaReadCapBytes() int64 {
	return corpusfs.ResolveReadCapBytes(w.MaxFileBytes)
}

// readWholeMedia reads the object at ref through the corpus filesystem under a
// hard byte bound of maxBytes (#830). Callers within a batch should go through
// mediaBatchCache.readWholeMedia so the read is shared across sibling chunks of
// the same MediaRef (issue #279).
//
// The bound is on the READ, and that is the whole point. Discovery measured the
// file with a stat (local) or a list entry (S3) many minutes earlier, and chunk
// rows outlive that measurement: a file that grew after it was chunked is read
// HERE, into memory, whole, for the embed call. A repeated size check would only
// produce a second measurement, so it cannot constrain this read; a limit reader
// can, and it is the only thing that catches a local file that grew.
//
// It reads at most maxBytes+1 bytes. The extra byte is the only evidence that
// separates a file of exactly the cap (inside the operator's policy, embed it)
// from a file one byte past it (refuse it). Past the cap the prefix is DROPPED
// and corpusfs.ErrObjectTooLarge is returned, so no caller can embed part of a
// file as though it were the whole of it.
//
// The error is deliberately not ErrFatal. An over-cap file is one document's
// problem, and ErrFatal stops the worker for the whole corpus. Left non-fatal and
// non-transient, the batch bisects, this chunk alone is marked failed with a
// reason that names the cap, and its healthy siblings still embed.
func readWholeMedia(ctx context.Context, fsys corpusfs.CorpusFS, ref string, maxBytes int64) ([]byte, error) {
	rc, err := fsys.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w (cap %d bytes)", corpusfs.ErrObjectTooLarge, maxBytes)
	}
	return data, nil
}

// loadPDFPage extracts the chunk's single page (from its `page` span) into a
// one-page PDF (SPEC 8.1.7). A missing/invalid page span is a fatal task error
// so a span/content mismatch surfaces instead of silently embedding page 1.
//
// Whole-object read (issue #243, deliberate): pdfutil.ExtractPage operates on the
// whole file. Passing a CorpusFS.Open io.ReadSeeker straight through to pdfcpu to
// let S3 range-GET only the page's objects was evaluated and rejected: pdfcpu's
// Trim parses the full cross-reference table and re-reads the file many times over
// (measured reaching 100% of the object and ~36x the file size in total bytes),
// so a ReadSeeker path would issue a storm of range GETs each re-fetching most of
// the object — strictly worse than one whole-object download. We therefore keep
// the whole-file read, but it is read exactly once per PDF per batch via the
// per-batch cache (issue #279), so every page of one PDF shares a single Open.
// Audio/video, by contrast, DO range-read on S3 (see loadMediaSegment) because
// ffmpeg can byte-range-seek a presigned URL.
func (w *EmbeddingWorker) loadPDFPage(ctx context.Context, t model.ChunkTask, fsys corpusfs.CorpusFS, ref string, cache *mediaBatchCache) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(t.Metadata.Span.Kind), "page") || t.Metadata.Span.Page < 1 {
		return nil, fmt.Errorf("%w: pdf media chunk %d has invalid page span", ErrFatal, t.Metadata.ChunkID)
	}
	data, err := cache.readWholeMedia(ctx, fsys, ref, w.mediaReadCapBytes())
	if err != nil {
		return nil, fmt.Errorf("read media %q: %w", ref, err)
	}
	pageData, perr := pdfutil.ExtractPage(data, t.Metadata.Span.Page)
	if perr != nil {
		return nil, fmt.Errorf("%w: %v", ErrFatal, perr)
	}
	return pageData, nil
}

// loadMediaSegment cuts the chunk's single time window (from its `time` span)
// out of the source media (SPEC 8.1.7). A missing/invalid time span is a fatal
// task error so a span/content mismatch surfaces instead of embedding the wrong
// segment.
//
// Range-read (issue #243): when the corpus backend exposes a range-seekable URL
// (S3FS via MediaURLProvider/presigned GetObject), ffmpeg reads the window over
// HTTP and pulls only the bytes around [start,end) plus the container index — the
// whole object is NOT downloaded. The presigned URL is memoized per MediaRef in
// the per-batch cache (issue #279), so every time-window of one source presigns
// once. When the backend cannot produce a URL (LocalFS, or an S3FS built without
// a presigner) the worker falls back to Localize (a whole-object download for S3,
// a no-op for LocalFS) because ffmpeg cannot read from an io.Reader — that path
// is byte-for-byte the historical behavior.
//
// Failures on the range-read URL path (presign or extract) are wrapped retryable
// (errRetryableMediaRead), not fatal: an expired/throttled presigned URL recovers
// on the next cycle once a fresh URL is minted, so the chunk is left pending. The
// fallback Localize/extract path keeps its historical ErrFatal classification.
func (w *EmbeddingWorker) loadMediaSegment(ctx context.Context, t model.ChunkTask, fsys corpusfs.CorpusFS, ref string, cache *mediaBatchCache) ([]byte, error) {
	span := t.Metadata.Span
	if !strings.EqualFold(strings.TrimSpace(span.Kind), "time") || span.StartMS < 0 || span.EndMS <= span.StartMS {
		return nil, fmt.Errorf("%w: %s media chunk %d has invalid time span", ErrFatal, strings.ToLower(t.Modality), t.Metadata.ChunkID)
	}

	// Prefer a range-seekable URL when the backend can presign one. A presign
	// failure is transient (throttling, a momentary credential refresh), so it is
	// marked retryable to keep the chunk pending rather than permanently failed.
	if url, ok, uerr := cache.mediaURL(ctx, fsys, ref); uerr != nil {
		return nil, fmt.Errorf("%w: presign media %q: %v", errRetryableMediaRead, ref, uerr)
	} else if ok {
		return w.extractSegmentFromURL(ctx, url, ref, span)
	}

	// Fall back to a localized path (whole-object download on S3, no-op locally).
	return w.extractSegmentFromPath(ctx, fsys, ref, span, cache)
}

// extractSegmentFromURL cuts the [start,end) window from a range-seekable URL via
// ffmpeg-over-HTTP. srcExt is derived from the MediaRef so the clip keeps the
// source container/MIME (a presigned URL's path/query is not a reliable hint).
//
// A failure here is NOT wrapped as ErrFatal (unlike the local-path extractor): a
// range-read over HTTP can fail for transient reasons that a later cycle would
// recover from — most importantly an expired presigned URL (the per-batch cache
// memoizes one URL with a 15-min lifetime, issue #243/#279), but also throttling
// or a network blip. Marking such a chunk permanently failed would silently lose
// it; returning a retryable error leaves it pending so the next cycle presigns a
// fresh URL. ref (the relPath, never the signed URL) is the only identifier put
// in the error so the credential cannot leak (the inner avutil error is already
// redacted).
func (w *EmbeddingWorker) extractSegmentFromURL(ctx context.Context, url, ref string, span model.Span) ([]byte, error) {
	extract := w.ExtractSegmentURLFunc
	if extract == nil {
		extract = avutil.ExtractSegmentURL
	}
	data, err := extract(ctx, url, filepath.Ext(ref), span.StartMS, span.EndMS)
	if err != nil {
		return nil, fmt.Errorf("%w: extract %q segment [%d,%d) over range-read URL: %v", errRetryableMediaRead, ref, span.StartMS, span.EndMS, err)
	}
	return data, nil
}

// extractSegmentFromPath cuts the [start,end) window from a localized filesystem
// path (the historical, whole-object-download-on-S3 fallback).
func (w *EmbeddingWorker) extractSegmentFromPath(ctx context.Context, fsys corpusfs.CorpusFS, ref string, span model.Span, cache *mediaBatchCache) ([]byte, error) {
	localPath, cleanup, err := cache.localize(ctx, fsys, ref)
	if err != nil {
		return nil, fmt.Errorf("read media %q: %w", ref, err)
	}
	defer cleanup()
	extract := w.ExtractSegmentFunc
	if extract == nil {
		extract = avutil.ExtractSegment
	}
	data, err := extract(ctx, localPath, span.StartMS, span.EndMS)
	if err != nil {
		return nil, fmt.Errorf("%w: extract %q segment [%d,%d): %v", ErrFatal, ref, span.StartMS, span.EndMS, err)
	}
	return data, nil
}

// isMediaModality reports whether a chunk modality denotes embeddable media.
func isMediaModality(modality string) bool {
	switch strings.ToLower(strings.TrimSpace(modality)) {
	case "image", "audio", "video", "pdf":
		return true
	default:
		return false
	}
}

// mediaMIMEType maps a file extension to a MIME type for multimodal
// embedding, defaulting to application/octet-stream.
func mediaMIMEType(relPath string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(relPath), ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	case "pdf":
		return "application/pdf"
	case "mp3":
		return "audio/mp3"
	case "wav":
		return "audio/wav"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	default:
		return "application/octet-stream"
	}
}

// payloadFromTask projects a chunk task's metadata into the IndexPayload the
// index stores alongside the vector (issue #247). The span's time bounds are
// surfaced as StartMS/EndMS so a backend filtering on media windows has them
// without re-reading the span; Speaker/SpeakerLabel are likewise surfaced from a
// diarized transcript's "time" span (SPEC §8.6.8) so a FilteringIndex can push
// down a speaker filter without re-reading the span. They are empty on every
// non-diarized transcript, so payloads are byte-identical to today. Language is
// the representation's effective BCP-47 language (SPEC §5.2/§8.8) so a
// FilteringIndex can push down the per-language filter (§9.5); it is empty on a
// representation that recorded no language (unknown), preserving prior payloads.
func payloadFromTask(t model.ChunkTask) model.IndexPayload {
	meta := t.Metadata
	return model.IndexPayload{
		ChunkID:      meta.ChunkID,
		RelPath:      meta.RelPath,
		DocType:      meta.DocType,
		RepType:      meta.RepType,
		Modality:     meta.Modality,
		Title:        meta.Title,
		StartMS:      meta.Span.StartMS,
		EndMS:        meta.Span.EndMS,
		Speaker:      meta.Span.Speaker,
		SpeakerLabel: meta.Span.SpeakerLabel,
		Snippet:      meta.Snippet,
		Span:         meta.Span,
		MediaRef:     meta.MediaRef,
		Language:     meta.Language,
	}
}

// indexChunks adds each vector to the index and fires the OnIndexedChunk hook.
// On an index error it marks the failed chunk and, if any chunks were already
// added, marks those as embedded first.
//
// When the backend implements model.BatchUpserter (issue #429 F8) the whole
// batch is applied in one call first: the on-disk backend fsyncs once per batch
// there instead of once per chunk, which is what caps disk-backend ingest at the
// fsync rate. The batch is a pure fast path: any batch error falls through to
// the per-chunk loop below, which reproduces the existing error contract exactly
// (partial MarkEmbedded, transient-vs-terminal classification, per-chunk
// MarkFailedWithCategory) by finding the offending chunk itself. Replaying is
// safe because BatchUpserter requires a failed batch to leave the index
// replayable (the disk backend rolls its segment back).
//
// Durability note: with the batch path a chunk's vector is durable per batch
// rather than per chunk. The invariant that matters is unchanged, because BatchUpsert
// returns only after its durability barrier, and chunks are marked embedded in
// the store strictly after indexChunks returns, so a crash can never leave the
// store claiming a chunk is embedded whose vector was lost; those chunks stay
// pending and are re-embedded on the next run.
func (w *EmbeddingWorker) indexChunks(ctx context.Context, validTasks []model.ChunkTask, labels []uint64, vectors [][]float32) (int, error) {
	if batcher, ok := w.Index.(model.BatchUpserter); ok {
		if w.batchIndexChunks(ctx, batcher, validTasks, vectors) {
			return len(labels), nil
		}
	}
	return w.upsertChunksSerially(ctx, validTasks, labels, vectors)
}

// batchIndexChunks applies every vector through the backend's batch path and
// reports whether it succeeded. On success it fires OnIndexedChunk in task order,
// exactly as the per-chunk loop does; on failure it reports false so the caller
// can replay per chunk and attribute the error.
func (w *EmbeddingWorker) batchIndexChunks(ctx context.Context, batcher model.BatchUpserter, validTasks []model.ChunkTask, vectors [][]float32) bool {
	items := make([]model.IndexUpsert, len(validTasks))
	for idx := range validTasks {
		items[idx] = model.IndexUpsert{Vector: vectors[idx], Payload: payloadFromTask(validTasks[idx])}
	}
	if err := batcher.BatchUpsert(ctx, items); err != nil {
		w.logf("batch upsert of %d chunks failed (%v); replaying per chunk to isolate the offender", len(items), err)
		return false
	}
	for idx := range validTasks {
		if w.OnIndexedChunk != nil {
			w.OnIndexedChunk(validTasks[idx].Metadata.ChunkID, validTasks[idx].Metadata)
		}
	}
	return true
}

// upsertChunksSerially is the per-chunk index path: the fallback for backends
// without model.BatchUpserter and the replay that attributes a failed batch to a
// specific chunk. It owns the index-error contract described on indexChunks.
func (w *EmbeddingWorker) upsertChunksSerially(ctx context.Context, validTasks []model.ChunkTask, labels []uint64, vectors [][]float32) (int, error) {
	for idx := range validTasks {
		if addErr := w.Index.Upsert(ctx, vectors[idx], payloadFromTask(validTasks[idx])); addErr != nil {
			if idx > 0 {
				if err := w.Source.MarkEmbedded(ctx, labels[:idx]); err != nil {
					w.logf("mark embedded warning: failed to mark %d chunks as embedded before index error: %v labels=%v", idx, err, labels[:idx])
				}
			}
			// A transient index/upsert error (the vector store is momentarily
			// unreachable — connection refused/reset, 503/529, EOF) must NOT
			// permanently fail the chunk: leave it PENDING so the next cycle
			// retries it, mirroring the embed-path behavior (issue #412).
			// Without this guard a brief Qdrant/pgvector blip silently buried
			// chunks in the error state corpus-wide.
			if isTransientEmbedError(addErr) {
				return idx, addErr
			}
			category := string(store.ClassifyError(addErr))
			reason := store.SanitizeReason(addErr.Error())
			if mfErr := w.Source.MarkFailedWithCategory(ctx, labels[idx:idx+1], category, reason); mfErr != nil {
				w.logf("mark failed update error: %v (index error: %v) labels=%v", mfErr, addErr, labels[idx:idx+1])
			}
			return idx, addErr
		}
		if w.OnIndexedChunk != nil {
			w.OnIndexedChunk(validTasks[idx].Metadata.ChunkID, validTasks[idx].Metadata)
		}
	}
	return len(labels), nil
}

// markEmbeddedWithRetry calls MarkEmbedded with exponential-backoff retries
// so that a transient DB hiccup does not cause already-indexed vectors to be
// re-indexed on the next cycle.
func (w *EmbeddingWorker) markEmbeddedWithRetry(ctx context.Context, labels []uint64) error {
	const maxRetries = 3
	retryDelay := 100 * time.Millisecond
	var meErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		meErr = w.Source.MarkEmbedded(ctx, labels)
		if meErr == nil {
			return nil
		}
		w.logf("mark embedded attempt %d/%d failed: %v labels=%v", attempt+1, maxRetries, meErr, labels)
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
			retryDelay *= 2
		}
	}
	w.logf("mark embedded final failure after %d attempts: %v labels=%v", maxRetries, meErr, labels)
	return meErr
}

func (w *EmbeddingWorker) RunOnce(ctx context.Context, indexKind string) (int, error) {
	if err := w.validate(); err != nil {
		return 0, err
	}

	w.logLateChunkDecisionOnce()

	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 32
	}

	tasks, err := w.Source.NextPending(ctx, batchSize, indexKind)
	if err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}
	return w.EmbedAndIndex(ctx, indexKind, tasks)
}

// EmbedAndIndex embeds the supplied chunk tasks for indexKind, upserts their
// vectors into the index, and marks them embedded (or failed) in the source —
// the shared embed→index→mark step extracted from RunOnce so a distributed
// embed-worker (SPEC §8.7) can reuse the exact same path on jobs it leases from
// a broker instead of polling NextPending. Writes are idempotent because the
// index upserts by chunk_id (SPEC §8.7.3), so re-running on an already-embedded
// chunk overwrites the same vector and re-sets the same terminal status — no
// duplicate vectors. Returns the number of chunks successfully indexed.
func (w *EmbeddingWorker) EmbedAndIndex(ctx context.Context, indexKind string, tasks []model.ChunkTask) (int, error) {
	if err := w.validate(); err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}
	// sanity-check tasks returned by the source.  they should already be
	// consistent, but validating here guards against misbehaving or
	// hand‑constructed implementations.
	if err := validateChunkTasks(tasks); err != nil {
		return 0, err
	}

	modelName := w.modelForKind(indexKind)
	validTasks, _, labels, err := buildEmbedBatch(tasks)
	if err != nil {
		return 0, err
	}
	// Skip oversized text inputs before sending: a single anomalous chunk (a
	// giant unbroken line/table far past the model's max input tokens) is
	// contained with a clear skip-reason instead of 400ing the provider and
	// poisoning the batch (issue #399 item 2). Its healthy siblings still embed.
	validTasks, labels = w.skipOversizeText(ctx, validTasks, labels)
	if len(validTasks) == 0 {
		return 0, nil
	}
	return w.embedIndexBatch(ctx, indexKind, modelName, validTasks, labels)
}

// defaultMaxEmbedInputRunes is the built-in per-input rune cap used when
// EmbeddingWorker.MaxInputRunes is unset. ~32k runes ≈ 8k tokens, at/above the
// max input of the common text-embedding models (OpenAI text-embedding-3 8191,
// Mistral 8192), so a normal chunk — the chunker targets far fewer tokens —
// always passes and only a genuinely anomalous input is skipped.
const defaultMaxEmbedInputRunes = 32000

// skipOversizeText partitions tasks into those within the per-input rune cap
// (returned, aligned 1:1 with their labels) and oversized TEXT chunks, which it
// marks embedding_failure with a clear skip-reason and drops from the batch
// (issue #399 item 2). Skipping up-front — rather than letting the provider 400
// and relying on bisection to isolate it — avoids the wasted round-trip and
// keeps a single anomalous chunk from ever threatening its siblings. Media
// chunks are exempt (they embed from bytes, not text; their size is bounded by
// the provider's byte ceiling). A MarkFailed write error is logged but not
// fatal: the chunk simply stays pending for a later cycle, never mis-embedded.
func (w *EmbeddingWorker) skipOversizeText(ctx context.Context, tasks []model.ChunkTask, labels []uint64) ([]model.ChunkTask, []uint64) {
	maxRunes := w.MaxInputRunes
	if maxRunes <= 0 {
		maxRunes = defaultMaxEmbedInputRunes
	}
	keptTasks := make([]model.ChunkTask, 0, len(tasks))
	keptLabels := make([]uint64, 0, len(labels))
	var oversize []uint64
	for i, t := range tasks {
		// Measure the input actually sent to the provider (the contextualized text
		// when contextual retrieval is on), so the oversize pre-skip cannot be
		// under-counted into a provider-rejected batch.
		if !isMediaModality(t.Modality) && utf8.RuneCountInString(t.EmbedInput()) > maxRunes {
			oversize = append(oversize, labels[i])
			continue
		}
		keptTasks = append(keptTasks, t)
		keptLabels = append(keptLabels, labels[i])
	}
	if len(oversize) > 0 {
		reason := fmt.Sprintf("input exceeds max embed input size of %d runes; skipped to avoid poisoning the batch", maxRunes)
		w.logf("skipping %d oversize text chunk(s) (> %d runes) [kind labels=%v]: %s", len(oversize), maxRunes, oversize, reason)
		if mfErr := w.Source.MarkFailedWithCategory(ctx, oversize, string(store.ErrorCategoryEmbeddingFailure), reason); mfErr != nil {
			w.logf("mark oversize failed update error: %v labels=%v", mfErr, oversize)
		}
	}
	return keptTasks, keptLabels
}

// embedIndexBatch embeds validTasks (aligned 1:1 with labels), upserts the
// resulting vectors into the index, and marks the chunks embedded. It is the
// recursive core of EmbedAndIndex.
//
// On a non-transient, non-fatal batch embed error it bisects the batch
// (bisectEmbedBatch) so a single provider-rejected "poison" input — an
// oversized / bad-encoding / over-token-limit chunk that 400s the whole HTTP
// request, since openai/gemini/omniembed all embed by batch — is isolated and
// marked failed while its healthy siblings still embed (issue #399). Previously
// one bad chunk marked up to batch-size-1 healthy siblings embedding_failure.
//
// The classification contract from issue #412 is preserved: transient errors
// (network hiccup, rate limit, cancellation) leave the chunks PENDING for a
// later cycle and are never marked failed; ErrFatal is a local media/config
// failure (not a provider input rejection) and keeps the existing whole-batch
// behavior so genuine misconfiguration stays loud rather than degrading into a
// silent per-chunk failure storm.
func (w *EmbeddingWorker) embedIndexBatch(ctx context.Context, indexKind, modelName string, validTasks []model.ChunkTask, labels []uint64) (int, error) {
	// Text chunks embed via Embed; media chunks (SPEC 8.1.7) embed via
	// EmbedMedia. embedTasks returns vectors aligned 1:1 with validTasks.
	vectors, err := w.embedTasks(ctx, modelName, validTasks)
	if err != nil {
		// A transient error could be a network timeout, rate-limit response, or
		// context cancellation. Returning it without marking the chunks failed
		// leaves them PENDING so they are re-fetched on the next cycle (#412).
		if isTransientEmbedError(err) {
			return 0, err
		}
		// Non-transient, multi-item, not a local/config fatal, and not a
		// corpus-wide auth rejection: this is the poison-chunk case. Bisect the
		// batch so only the offending input(s) fail instead of the whole HTTP
		// batch (#399). Auth failures (401/403) are excluded because they reject
		// every input identically — bisecting them isolates nothing and instead
		// explodes one batch into O(n) embed calls (1+2+4+…), hammering the
		// provider and risking a rate-limit/lockout, so they keep the existing
		// whole-batch failure behavior below.
		if len(validTasks) > 1 && !errors.Is(err, ErrFatal) &&
			store.ClassifyError(err) != store.ErrorCategoryAuth {
			return w.bisectEmbedBatch(ctx, indexKind, modelName, validTasks, labels, err)
		}
		// Single item, a fatal batch error, or an auth batch: mark failed
		// (existing behavior).
		category := string(store.ClassifyError(err))
		reason := store.SanitizeReason(err.Error())
		if mfErr := w.Source.MarkFailedWithCategory(ctx, labels, category, reason); mfErr != nil {
			w.logf("mark failed update error: %v (source error: %v) labels=%v", mfErr, err, labels)
		}
		// ErrFatal is a local media/config failure that must stay loud and stop
		// the worker (issue #412), so propagate it unwrapped — a parent
		// bisectEmbedBatch must NOT absorb it. Every other non-transient failure
		// here has been terminally marked failed, so wrap it in the
		// errChunkMarkedFailed sentinel: a parent bisection absorbs that (its
		// healthy siblings still succeed) while the run loop can still tell it
		// apart from a post-embed write failure that must propagate.
		if errors.Is(err, ErrFatal) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %w", errChunkMarkedFailed, err)
	}
	if len(vectors) != len(validTasks) {
		reason := "embedding vector count mismatch"
		if mfErr := w.Source.MarkFailedWithCategory(ctx, labels, string(store.ErrorCategoryEmbeddingFailure), reason); mfErr != nil {
			w.logf("mark failed update error: %v (reason: %s) labels=%v", mfErr, reason, labels)
		}
		// Terminally marked failed, so absorbable by a parent bisection (same
		// contract as the embed-rejection case above).
		return 0, fmt.Errorf("%w: %s", errChunkMarkedFailed, reason)
	}

	n, err := w.indexChunks(ctx, validTasks, labels, vectors)
	if err != nil {
		return n, err
	}

	// Attempt to mark all successfully indexed chunks as embedded.
	// Because the vectors are already in the index, a transient DB hiccup
	// should not cause them to be re-indexed on the next cycle – so retry
	// with exponential backoff before giving up.
	if err := w.markEmbeddedWithRetry(ctx, labels); err != nil {
		return len(labels), err
	}

	return len(labels), nil
}

// bisectEmbedBatch isolates the provider-rejected input(s) in a batch that
// failed a non-transient, non-fatal embed by splitting it in half and
// re-embedding each half through embedIndexBatch. Recursion narrows to the
// individual poison chunk, which is marked failed (embedding_status=error)
// while every healthy sibling still embeds and indexes (issue #399).
//
// Error precedence in the return value (see mergeBisectErrors): only the
// errChunkMarkedFailed sentinel is absorbed — an isolated poison chunk this
// recursion already marked failed is terminal, so once it is contained the batch
// reports success for the healthy remainder. Every other error propagates:
// transient errors (those chunks stayed PENDING, so the run loop must back off
// and re-fetch them), ErrFatal (a local media/config failure that must stop the
// worker, issue #412 — and it wins over a sibling's transient error), and
// post-embed write failures from indexChunks/markEmbeddedWithRetry (the run loop
// must see them or the affected chunks re-embed in a tight loop).
// Returns the total number of chunks successfully indexed across both halves.
func (w *EmbeddingWorker) bisectEmbedBatch(ctx context.Context, indexKind, modelName string, validTasks []model.ChunkTask, labels []uint64, batchErr error) (int, error) {
	w.logf("embed batch of %d failed non-transiently (%s); bisecting to isolate poison chunk(s) so healthy siblings still embed [kind=%s]",
		len(validTasks), store.SanitizeReason(batchErr.Error()), indexKind)
	mid := len(validTasks) / 2
	nLeft, errLeft := w.embedIndexBatch(ctx, indexKind, modelName, validTasks[:mid], labels[:mid])
	nRight, errRight := w.embedIndexBatch(ctx, indexKind, modelName, validTasks[mid:], labels[mid:])
	return nLeft + nRight, mergeBisectErrors(errLeft, errRight)
}

// mergeBisectErrors selects the error a bisection should surface to its caller.
// The errChunkMarkedFailed sentinel (a poison chunk a recursion already marked
// terminally failed) is absorbed so healthy siblings still report success. Of
// the remaining errors ErrFatal takes precedence — a local media/config failure
// must stop the worker (issue #412) even when a sibling half only saw a
// transient error — otherwise the first non-absorbable error is returned.
func mergeBisectErrors(errs ...error) error {
	var first error
	for _, e := range errs {
		if e == nil || errors.Is(e, errChunkMarkedFailed) {
			continue
		}
		if errors.Is(e, ErrFatal) {
			return e
		}
		if first == nil {
			first = e
		}
	}
	return first
}

// Run starts a background loop that periodically calls RunOnce. A small
// tick interval is used to check for context cancellation and to space
// invocations; the caller may choose a large interval if they only want to
// poll infrequently. If RunOnce returns an error the behaviour depends on
// whether the error is retryable. Retryable errors are logged and the
// method sleeps with exponential backoff before trying again. Fatal errors
// are logged, propagated via ErrCh (if provided) and cause Run to return.
//
// Note that Run does not attempt to restart itself if a fatal error occurs;
// callers that want resilient workers should either monitor ErrCh or simply
// re‑invoke Run in a supervising goroutine.
func (w *EmbeddingWorker) Run(ctx context.Context, interval time.Duration, indexKind string) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// start backoff at the same interval passed in so tests using very small
	// intervals won't sleep for a full second on the first retry.
	// interval is guaranteed positive above, so we can assign directly.
	backoff := interval
	const maxBackoff = 30 * time.Second

	// One startup line so the support bundle / server.log shows the worker is
	// actually running for this axis. Its ABSENCE on a corpus with pending
	// chunks (embedded_pending>0) is itself the diagnosis. See issue #364.
	w.logf("embed worker started [kind=%s, interval=%s]", indexKind, interval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// allow an override for testing or specialised behaviour
			runOnce := w.RunOnce
			if w.RunOnceFunc != nil {
				runOnce = w.RunOnceFunc
			}
			n, err := runOnce(ctx, indexKind)
			if n > 0 {
				// Progress is otherwise invisible: a corpus that never logs
				// "embedded N" while embedded_pending stays high tells us the
				// worker found nothing to do (filter/visibility) vs. embedded
				// fine (which would show here).
				w.logf("embedded %d chunk(s) [kind=%s]", n, indexKind)
			}
			if err != nil {
				if isRetryable(err) {
					w.logf("run once failed (retryable): %v; backing off %v", err, backoff)
					// wait either for context cancel or the backoff timer
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(backoff):
					}
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				}
				// fatal
				w.logf("run once failed (fatal): %v", err)
				if w.ErrCh != nil {
					select {
					case w.ErrCh <- err:
					default:
					}
				}
				return err
			}
			// success, reset backoff to minimum
			backoff = interval
		}
	}
}

// logf is a small helper that routes messages to the configured logger or
// the global log package.
func (w *EmbeddingWorker) logf(format string, args ...interface{}) {
	if w != nil && w.Logger != nil {
		w.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// ErrFatal can be returned by RunOnce to signal that the worker should
// not retry and should exit immediately. It is exported so callers can wrap
// or compare against it if they produce fatal conditions themselves.
var ErrFatal = errors.New("fatal")

// errRetryableMediaRead marks a media fetch/extract failure that a later cycle
// could recover from, so the affected chunk must stay pending (not be marked
// permanently failed). The canonical case is an S3-backed range-read whose
// presigned URL expired or was throttled mid-batch (issue #243): the per-batch
// cache holds one 15-min URL, and although that is comfortably longer than a
// default batch of stream-copy windows takes, a slow network / clock skew / a
// large batch override could outlast it — and re-presigning on the next cycle
// fixes it. Recognized by isTransientEmbedError so it routes to the retry (keep
// pending) path rather than MarkFailed.
var errRetryableMediaRead = errors.New("retryable media read")

// errChunkMarkedFailed marks a batch whose chunk(s) embedIndexBatch has already
// moved to a terminal failed state (embedding_status=error) — the isolated
// poison chunk from bisection (#399) or a vector-count-mismatch. It exists so
// bisectEmbedBatch can absorb exactly this case (its healthy siblings still
// succeed) while still propagating everything the run loop must act on: transient
// errors, ErrFatal, and post-embed write failures from indexChunks /
// markEmbeddedWithRetry. It is never returned for a transient error or for
// ErrFatal (both propagate unwrapped), so absorbing it cannot swallow those.
var errChunkMarkedFailed = errors.New("chunk marked failed")

// isRetryable determines whether RunOnce should be retried when it returns
// the provided error. The predicate is intentionally conservative; context
// cancellation, deadline errors, and ErrFatal are considered fatal because
// re‑running after they have occurred is unlikely to succeed.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrFatal) {
		return false
	}
	return true
}

// isTransientEmbedError categorises errors returned by an Embedder.  If the
// error is considered transient the worker should not mark the associated
// chunks as failed; the caller can simply return the error and the chunk
// will remain pending.  The heuristics here are intentionally conservative –
// anything that looks like a network hiccup, timeout, rate limit, or
// cancellation is treated as transient.  Other errors are assumed to be
// permanent and callers may safely mark the work item failed.
func isTransientEmbedError(err error) bool {
	if err == nil {
		return false
	}
	// A range-read media fetch/extract failure (e.g. an expired or throttled
	// presigned URL, issue #243) is recoverable on a later cycle, so keep the
	// chunk pending instead of marking it permanently failed.
	if errors.Is(err, errRetryableMediaRead) {
		return true
	}
	// context package errors are usually propagated from the caller and
	// indicate the operation stopped; leave the chunk pending rather than
	// declare it failed.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// A bare EOF / unexpected-EOF (connection dropped mid-response) is a
	// transient transport failure that a later cycle recovers from. Match it
	// structurally (errors.Is) in addition to the keyword set below, since
	// io.EOF survives wrapping (e.g. *model.ProviderError.Cause) where the
	// string form may not.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// net.Error can indicate timeouts or temporary network failures.
	var ne net.Error
	if errors.As(err, &ne) {
		// timeout errors are almost always transient
		if ne.Timeout() {
			return true
		}
		// Temporary() is deprecated (see staticcheck) but in practice a
		// few drivers/clients still return it to indicate retryable network
		// glitches that are not strictly timeouts.  We check it here and
		// silence the linter rather than accidentally dropping those cases.
		if ne.Temporary() { //nolint:staticcheck
			return true
		}
	}
	// Rate-limit responses are retryable after backoff. store.IsTransientError
	// (below) classifies rate limits under their own category, not transient_net,
	// so keep this check explicit here.
	if strings.Contains(strings.ToLower(err.Error()), "rate limit") {
		return true
	}
	// Defer to the shared store classification so the worker's retry gate and
	// the transient_net diagnostics label agree on exactly the same set of
	// errors (issue #412): timeouts, connection refused/reset, DNS failures,
	// 503/529, service unavailable, overloaded, EOF. A failure left PENDING
	// here is the same class dir2mcp status/doctor report as transient_net.
	return store.IsTransientError(err)
}

// modelForKind returns the configured embed model for the index axis, or ""
// when none was resolved. An empty result is intentional: it tells the
// provider ADAPTER to apply its own correct default (text-embedding-3-small
// for OpenAI, embed-v4.0 for Cohere, gemini-embedding-001 for Gemini, etc.).
// Previously this fell back to the Mistral model names ("mistral-embed" /
// "codestral-embed"), which silently sent a Mistral model id to whatever
// provider was actually active — so swapping the embed provider to OpenAI or
// Cohere failed the whole corpus with an unknown-model error (issue #396). The
// concrete model is supplied upstream from the resolved embed profile
// (ModelForText/ModelForCode); only when that profile leaves it blank do we
// defer to the adapter default.
func (w *EmbeddingWorker) modelForKind(indexKind string) string {
	if strings.ToLower(strings.TrimSpace(indexKind)) == "code" {
		return strings.TrimSpace(w.ModelForCode)
	}
	return strings.TrimSpace(w.ModelForText)
}

// LateChunkDecision resolves the late-chunking capability gate (issue #332) for
// this worker's configured embedder: it returns whether late chunking is active
// (enabled AND the embedder exposes token-level embeddings via
// model.TokenEmbedder) and, when not, the fallback reason. It is the single
// routing point so the embed step and tests agree on the decision.
func (w *EmbeddingWorker) LateChunkDecision() latechunk.Decision {
	return latechunk.Decide(w.LateChunking, w.Embedder)
}

// logLateChunkDecisionOnce emits a single informational line describing the
// late-chunking decision the first time the worker runs with the feature
// enabled, so an operator who turned on ingest.late_chunking can see whether it
// actually engaged or fell back to chunk-then-embed (issue #332: never fail
// silently). It is a no-op when late chunking is disabled (the default), so
// stock runs log nothing new.
//
// Honesty note (issue #446): even when the capability gate is satisfied
// (dec.Active — the embedder exposes token embeddings), the production embed
// loop does NOT yet run the whole-doc-embed + mean-pool path, so the worker
// still embeds chunk-then-embed. The log therefore MUST NOT claim the path is
// "active" in that case (the previous message lied): it reports that the
// capability is present but the pooling path is not yet wired, so the fallback
// is in effect. When the token-pooling path is wired, restore an "enabled and
// active" line here (and branch the embed loop on dec.Active).
func (w *EmbeddingWorker) logLateChunkDecisionOnce() {
	if !w.LateChunking || w.lateChunkLogged {
		return
	}
	w.lateChunkLogged = true
	dec := w.LateChunkDecision()
	if dec.Active {
		w.logf("late chunking: enabled and embedder %T exposes token embeddings, but the token-pooling path is not yet wired (issue #446); still embedding chunk-then-embed", w.Embedder)
		return
	}
	w.logf("late chunking: enabled but falling back to chunk-then-embed (%s; embedder %T)", dec.Fallback, w.Embedder)
}
