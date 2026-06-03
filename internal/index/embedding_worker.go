package index

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
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

	// RootDir is the corpus root used to resolve a media chunk's MediaRef
	// (a corpus rel_path) to bytes for multimodal embedding (SPEC 8.1.7).
	// Required only when media chunks are present; text-only corpora may
	// leave it empty.
	RootDir string

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
		inputs = append(inputs, task.Text)
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

	textIdx := make([]int, 0, len(validTasks))
	textInputs := make([]string, 0, len(validTasks))
	mediaIdx := make([]int, 0)
	mediaItems := make([]model.MediaInput, 0)
	for i, t := range validTasks {
		if isMediaModality(t.Modality) {
			item, err := w.loadMediaInput(t)
			if err != nil {
				return nil, err
			}
			mediaIdx = append(mediaIdx, i)
			mediaItems = append(mediaItems, item)
			continue
		}
		textIdx = append(textIdx, i)
		textInputs = append(textInputs, t.Text)
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
	return vectors, nil
}

// loadMediaInput reads a media chunk's source bytes (resolved from RootDir +
// MediaRef) and infers its MIME type. The resolved path is constrained to
// RootDir as defense-in-depth against a traversal in a stored ref.
func (w *EmbeddingWorker) loadMediaInput(t model.ChunkTask) (model.MediaInput, error) {
	ref := strings.TrimSpace(t.MediaRef)
	if ref == "" {
		return model.MediaInput{}, fmt.Errorf("%w: media chunk %d has no media_ref", ErrFatal, t.Metadata.ChunkID)
	}
	if strings.TrimSpace(w.RootDir) == "" {
		return model.MediaInput{}, fmt.Errorf("%w: media embedding requires a corpus root", ErrFatal)
	}
	root, err := filepath.Abs(w.RootDir)
	if err != nil {
		return model.MediaInput{}, fmt.Errorf("%w: resolve root: %v", ErrFatal, err)
	}
	// Resolve symlinks on both the root and the target so a symlink *within*
	// the corpus that points outside it cannot smuggle out-of-root bytes in
	// (a lexical Join+HasPrefix check alone would miss that).
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root // root should exist; fall back to the lexical form
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(ref)))
	if err != nil {
		return model.MediaInput{}, fmt.Errorf("read media %q: %w", ref, err)
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
		return model.MediaInput{}, fmt.Errorf("%w: media_ref %q escapes the corpus root", ErrFatal, ref)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return model.MediaInput{}, fmt.Errorf("read media %q: %w", ref, err)
	}
	return model.MediaInput{MimeType: mediaMIMEType(ref), Data: data}, nil
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

// indexChunks adds each vector to the index and fires the OnIndexedChunk hook.
// On an index error it marks the failed chunk and, if any chunks were already
// added, marks those as embedded first.
func (w *EmbeddingWorker) indexChunks(ctx context.Context, validTasks []model.ChunkTask, labels []uint64, vectors [][]float32) (int, error) {
	for idx := range validTasks {
		if addErr := w.Index.Add(validTasks[idx].Metadata.ChunkID, vectors[idx]); addErr != nil {
			if idx > 0 {
				if err := w.Source.MarkEmbedded(ctx, labels[:idx]); err != nil {
					w.logf("mark embedded warning: failed to mark %d chunks as embedded before index error: %v labels=%v", idx, err, labels[:idx])
				}
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

	// Text chunks embed via Embed; media chunks (SPEC 8.1.7) embed via
	// EmbedMedia. embedTasks returns vectors aligned 1:1 with validTasks.
	vectors, err := w.embedTasks(ctx, modelName, validTasks)
	if err != nil {
		// distinguish between transient errors (which we want to retry later)
		// and permanent failures for which the chunks should be marked as
		// irrecoverable.  A transient error could be a network timeout,
		// rate‑limit response, or context cancellation.  We intentionally keep
		// the interface simple; by returning the error without marking the
		// chunks as failed they will remain in the pending state and be
		// re‑fetched on the next cycle.  Permanent errors fall through to the
		// existing MarkFailed behaviour.
		if isTransientEmbedError(err) {
			return 0, err
		}
		category := string(store.ClassifyError(err))
		reason := store.SanitizeReason(err.Error())
		if mfErr := w.Source.MarkFailedWithCategory(ctx, labels, category, reason); mfErr != nil {
			w.logf("mark failed update error: %v (source error: %v) labels=%v", mfErr, err, labels)
		}
		return 0, err
	}
	if len(vectors) != len(validTasks) {
		reason := "embedding vector count mismatch"
		if mfErr := w.Source.MarkFailedWithCategory(ctx, labels, string(store.ErrorCategoryEmbeddingFailure), reason); mfErr != nil {
			w.logf("mark failed update error: %v (reason: %s) labels=%v", mfErr, reason, labels)
		}
		return 0, errors.New(reason)
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
			_, err := runOnce(ctx, indexKind)
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
	// context package errors are usually propagated from the caller and
	// indicate the operation stopped; leave the chunk pending rather than
	// declare it failed.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
	// some embedder implementations return textual hints for rate limits or
	// timeouts; look for those substrings so the behaviour is still correct
	// even if they don't implement the net.Error interface.
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "timeout") {
		return true
	}
	return false
}

func (w *EmbeddingWorker) modelForKind(indexKind string) string {
	kind := strings.ToLower(strings.TrimSpace(indexKind))
	switch kind {
	case "code":
		if strings.TrimSpace(w.ModelForCode) != "" {
			return w.ModelForCode
		}
		return "codestral-embed"
	default:
		if strings.TrimSpace(w.ModelForText) != "" {
			return w.ModelForText
		}
		return "mistral-embed"
	}
}
