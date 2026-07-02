package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// distributedTaskFetcher is the read-by-id capability the distributed worker
// needs from the shared store (SPEC §8.7.4). The metadata store satisfies it.
type distributedTaskFetcher interface {
	ChunkTaskByID(ctx context.Context, chunkID uint64) (model.ChunkTask, string, error)
}

// startDistributedEmbedding runs the in-process degenerate case of the
// distributed coordinator+worker topology (issue #248, SPEC §8.7): it builds the
// configured broker, starts a coordinator that enqueues pending chunks, and a
// worker that drains the queue by reusing the exact in-process embedding path
// (index.EmbeddingWorker.EmbedAndIndex). This changes WHERE embedding runs (via a
// broker) but not WHAT is persisted; with distributed mode off this function is
// never reached and behavior is byte-identical to before.
//
// The full cross-machine deployment (a standalone worker process) lands in
// follow-up #249, which wraps embedqueue.Run in a CLI subcommand; this PR ships
// the reusable run-loop, not that subcommand.
func startDistributedEmbedding(
	ctx context.Context,
	cfg config.Config,
	st model.Store,
	chunkSource index.ChunkSource,
	textIndex, codeIndex model.Index,
	embedder model.Embedder,
	ret *retrieval.Service,
	indexingState *appstate.IndexingState,
	errCh chan<- error,
	logger *log.Logger,
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	wg *sync.WaitGroup,
) error {
	// Setup failures are returned (not sent to errCh) so the caller can FAIL FAST:
	// a server that cannot start its embedding topology would otherwise run
	// silently with nothing draining the pending queue.
	fetcher, ok := st.(distributedTaskFetcher)
	if !ok {
		return errors.New("distributed embedding: store does not support ChunkTaskByID")
	}

	identityStr := cfg.Providers().EmbedIdentity()
	if identityStr == "" {
		return errors.New("distributed embedding: embed identity could not be resolved")
	}

	broker, err := buildEmbedBroker(ctx, cfg)
	if err != nil {
		return fmt.Errorf("distributed embedding: build broker: %w", err)
	}

	// Build a per-kind embedder reusing the in-process embedding path so the
	// distributed worker shares all embed/media-load/index/mark logic.
	embedders := buildAxisEmbedders(chunkSource, textIndex, codeIndex, embedder, ret, indexingState, textModel, codeModel, rootDir, corpusFS, logger)
	if len(embedders) == 0 {
		// Post-open abort: close the broker so its SQLite handle is not leaked.
		_ = broker.Close()
		return errors.New("distributed embedding: no index axis configured")
	}

	coord := &embedqueue.Coordinator{
		Source:        chunkSource,
		Broker:        broker,
		CorpusID:      rootDir,
		SourceKind:    cfg.Source.Kind,
		EmbedIdentity: identityStr,
	}

	workerCfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetcher,
		Embedders:     embedders,
		EmbedIdentity: identityStr,
		Logger:        logger,
	}

	// Register every goroutine below on the caller's drain group so runUp's
	// deferred cancel()+Wait() blocks until they exit before it closes the
	// store/indices and returns — otherwise their logging (via logger → the shared
	// sink) can race a caller reading that sink after return, and the worker can
	// touch the store after Close (issue #419).
	spawn := func(fn func()) {
		if wg != nil {
			wg.Add(1)
		}
		go func() {
			if wg != nil {
				defer wg.Done()
			}
			fn()
		}()
	}

	// Close the broker when the run context ends so its handle (e.g. a SQLite DB)
	// is released on shutdown.
	spawn(func() {
		<-ctx.Done()
		_ = broker.Close()
	})

	// Coordinator: periodically enqueue chunks that are still pending. Each pass
	// enqueues the current pending head; the ticker drives the full drain as the
	// head clears and picks up chunks added later (incremental ingest), with no
	// global ordering requirement (SPEC §8.7.3).
	spawn(func() { runCoordinatorLoop(ctx, coord, errCh, logger) })

	// Worker: drain the queue. Errors are surfaced on errCh (cancellation is not
	// an error), mirroring startEmbeddingWorkers.
	spawn(func() {
		if rerr := embedqueue.Run(ctx, workerCfg); rerr != nil &&
			!errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
			errCh <- fmt.Errorf("distributed embed worker: %w", rerr)
		}
	})
	return nil
}

// runCoordinatorLoop enqueues pending chunks on a fixed interval until ctx is
// cancelled. Re-enqueuing is safe: already-embedded chunks are no longer pending,
// and a duplicate job is idempotent at the embed layer (SPEC §8.7.3).
func runCoordinatorLoop(ctx context.Context, coord *embedqueue.Coordinator, errCh chan<- error, logger *log.Logger) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := coord.EnqueuePending(ctx, ""); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if logger != nil {
				logger.Printf("distributed embedding: enqueue pending: %v", err)
			}
			// Also surface it as a structured event (the human logger is discarded
			// in JSON mode). Non-blocking: a transient enqueue error must not stall
			// the loop if errCh is full.
			select {
			case errCh <- fmt.Errorf("distributed embedding: enqueue pending: %w", err):
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// newEmbedStep builds an index.EmbeddingWorker configured for one axis and
// returns it as an embedqueue.Embedder (EmbedAndIndex). It reuses the identical
// embed/media-load/index/mark logic the in-process loop uses; only the source of
// tasks differs (leased jobs instead of NextPending polling).
func newEmbedStep(
	source index.ChunkSource,
	ix model.Index,
	embedder model.Embedder,
	ret *retrieval.Service,
	indexingState *appstate.IndexingState,
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	logger *log.Logger,
	kind string,
) *index.EmbeddingWorker {
	return &index.EmbeddingWorker{
		// Source provides the MarkEmbedded/MarkFailed* status writes EmbedAndIndex
		// performs (the in-process loop's exact write path). NextPending is unused
		// here — the distributed worker feeds tasks from leased jobs instead.
		Source:       source,
		Index:        ix,
		Embedder:     embedder,
		ModelForText: textModel,
		ModelForCode: codeModel,
		RootDir:      rootDir,
		Corpus:       corpusFS,
		BatchSize:    32,
		Logger:       logger,
		OnIndexedChunk: func(label uint64, metadata model.ChunkMetadata) {
			if ret != nil {
				ret.SetChunkMetadataForIndex(kind, label, metadata.ToSearchHit())
			}
			if indexingState != nil {
				indexingState.AddEmbeddedOK(1)
			}
		},
	}
}

// buildAxisEmbedders builds the per-axis (text/code) embed→index→mark steps a
// distributed worker drains jobs into, reusing newEmbedStep so each axis shares
// the exact in-process embed/media-load/index/mark path. A nil index for an axis
// is skipped (that axis has no embedder); a job whose index_kind has no embedder
// is dead-lettered by embedqueue.Run rather than mis-written. Shared by the
// in-process distributed coordinator (up) and the standalone embed-worker (#249).
func buildAxisEmbedders(
	chunkSource index.ChunkSource,
	textIndex, codeIndex model.Index,
	embedder model.Embedder,
	ret *retrieval.Service,
	indexingState *appstate.IndexingState,
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	logger *log.Logger,
) map[string]embedqueue.Embedder {
	embedders := make(map[string]embedqueue.Embedder)
	if textIndex != nil {
		embedders["text"] = newEmbedStep(chunkSource, textIndex, embedder, ret, indexingState, textModel, codeModel, rootDir, corpusFS, logger, "text")
	}
	if codeIndex != nil {
		embedders["code"] = newEmbedStep(chunkSource, codeIndex, embedder, ret, indexingState, textModel, codeModel, rootDir, corpusFS, logger, "code")
	}
	return embedders
}

// buildEmbedBroker constructs the configured broker (SPEC §8.7.4). The built-in
// "memory" and "sqlite" implementations are dependency-free; an external value
// would be dispatched to an adapter (not shipped in this PR — #248 ships the
// interface + defaults).
func buildEmbedBroker(ctx context.Context, cfg config.Config) (embedqueue.Broker, error) {
	switch cfg.DistributedEmbed.Broker {
	case "", "memory":
		return embedqueue.NewMemBroker(cfg.DistributedEmbed.MaxAttempts), nil
	case "sqlite":
		path := cfg.DistributedEmbed.BrokerSQLitePath
		if path == "" {
			path = filepath.Join(cfg.StateDir, "embed-queue.db")
		}
		return embedqueue.NewSQLiteBroker(ctx, path, cfg.DistributedEmbed.MaxAttempts)
	default:
		return nil, fmt.Errorf("unsupported distributed_embed.broker %q (built-ins: memory, sqlite)", cfg.DistributedEmbed.Broker)
	}
}
