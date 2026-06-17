package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
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
) {
	fetcher, ok := st.(distributedTaskFetcher)
	if !ok {
		errCh <- errors.New("distributed embedding: store does not support ChunkTaskByID")
		return
	}

	identityStr := cfg.Providers().EmbedIdentity()
	if identityStr == "" {
		errCh <- errors.New("distributed embedding: embed identity could not be resolved")
		return
	}

	broker, err := buildEmbedBroker(ctx, cfg)
	if err != nil {
		errCh <- fmt.Errorf("distributed embedding: build broker: %v", err)
		return
	}

	// Build a per-kind embedder reusing the in-process embedding path so the
	// distributed worker shares all embed/media-load/index/mark logic.
	embedders := make(map[string]embedqueue.Embedder)
	if textIndex != nil {
		embedders["text"] = newEmbedStep(chunkSource, textIndex, embedder, ret, indexingState, textModel, codeModel, rootDir, corpusFS, logger, "text")
	}
	if codeIndex != nil {
		embedders["code"] = newEmbedStep(chunkSource, codeIndex, embedder, ret, indexingState, textModel, codeModel, rootDir, corpusFS, logger, "code")
	}
	if len(embedders) == 0 {
		errCh <- errors.New("distributed embedding: no index axis configured")
		return
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

	// Coordinator: periodically enqueue chunks that are still pending. A single
	// pass drains the current backlog; a ticker picks up chunks added later
	// (incremental ingest) without a global ordering requirement (SPEC §8.7.3).
	go runCoordinatorLoop(ctx, coord, logger)

	// Worker: drain the queue. Errors are surfaced on errCh (cancellation is not
	// an error), mirroring startEmbeddingWorkers.
	go func() {
		if rerr := embedqueue.Run(ctx, workerCfg); rerr != nil &&
			!errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
			errCh <- fmt.Errorf("distributed embed worker: %w", rerr)
		}
	}()
}

// runCoordinatorLoop enqueues pending chunks on a fixed interval until ctx is
// cancelled. Re-enqueuing is safe: already-embedded chunks are no longer pending,
// and a duplicate job is idempotent at the embed layer (SPEC §8.7.3).
func runCoordinatorLoop(ctx context.Context, coord *embedqueue.Coordinator, logger *log.Logger) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := coord.EnqueuePending(ctx, ""); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if logger != nil {
				logger.Printf("distributed embedding: enqueue pending: %v", err)
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
