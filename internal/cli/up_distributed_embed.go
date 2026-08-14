package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/identity"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// distributedEmbedBatchSize is the number of chunks the distributed worker leases
// and embeds per iteration, and the per-axis EmbeddingWorker batch size (issue
// #435). Kept as one constant so the lease batch and the provider embed batch
// stay in lockstep; mirrors the in-process EmbeddingWorker default of 32.
const distributedEmbedBatchSize = 32

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

	corpusID, err := resolveCorpusID(ctx, cfg, st)
	if err != nil {
		return fmt.Errorf("distributed embedding: %w", err)
	}

	// Single-coordinator guard (issue #435 C3): refuse to start a second
	// coordinator for this corpus. The default broker is in-process and cannot
	// dedup across processes, so two daemons on one corpus would otherwise both
	// enqueue + double-embed the same chunks. The lock releases automatically if
	// this process dies (OS drops the advisory flock).
	coordLock, err := embedqueue.AcquireCoordinatorLock(filepath.Join(cfg.StateDir, "embed-coordinator.lock"))
	if err != nil {
		if errors.Is(err, embedqueue.ErrCoordinatorLocked) {
			return errors.New("distributed embedding: another dir2mcp is already embedding this corpus (coordinator lock held); refusing to start a second coordinator")
		}
		return fmt.Errorf("distributed embedding: acquire coordinator lock: %w", err)
	}

	broker, err := buildEmbedBroker(ctx, cfg)
	if err != nil {
		_ = coordLock.Release()
		return fmt.Errorf("distributed embedding: build broker: %w", err)
	}

	// Guard embedded_ok against redelivery double-counting (issue #435 C2): the
	// per-upsert hook fires on every successful (re-)embed, so gate the COUNT to a
	// chunk's first success. Shared across both axes' hooks.
	embeddedGuard := embedqueue.NewEmbeddedGuard()

	// Build a per-kind embedder reusing the in-process embedding path so the
	// distributed worker shares all embed/media-load/index/mark logic.
	embedders := buildAxisEmbedders(chunkSource, textIndex, codeIndex, embedder, ret, indexingState, embeddedGuard, textModel, codeModel, rootDir, corpusFS, logger, ingest.ResolvedMaxFileBytes(cfg))
	if len(embedders) == 0 {
		// Post-open abort: close the broker and release the lock so its SQLite
		// handle and the coordinator lock are not leaked.
		_ = broker.Close()
		_ = coordLock.Release()
		return errors.New("distributed embedding: no index axis configured")
	}

	coord := &embedqueue.Coordinator{
		Source:        chunkSource,
		Broker:        broker,
		CorpusID:      corpusID,
		SourceKind:    cfg.Source.Kind,
		EmbedIdentity: identityStr,
	}

	workerCfg := embedqueue.Config{
		Broker:    broker,
		Fetcher:   fetcher,
		Embedders: embedders,
		// The coordinator and the worker resolve the corpus id from the SAME store,
		// so the in-process pair is bound to one corpus by construction and a
		// broker shared with another corpus cannot cross-wire them (#708).
		CorpusID: corpusID,
		// Terminal per-chunk failures go through the same store the in-process loop
		// marks status on, so a dead-lettered job leaves the pending set instead of
		// being re-enqueued on the next coordinator tick (#709).
		Status:        chunkSource,
		EmbedIdentity: identityStr,
		// Lease/embed up to distributedEmbedBatchSize chunks per iteration so the
		// distributed path batches through the provider like the in-process loop
		// (one embed call per batch, not per chunk — issue #435). Kept in lockstep
		// with the per-axis EmbeddingWorker.BatchSize set in newEmbedStep.
		BatchSize: distributedEmbedBatchSize,
		Logger:    logger,
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

	// Close the broker and release the coordinator lock when the run context ends
	// so its handle (e.g. a SQLite DB) and the single-coordinator guard are
	// released on shutdown.
	spawn(func() {
		<-ctx.Done()
		_ = broker.Close()
		_ = coordLock.Release()
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

// resolveCorpusID returns the stable corpus identity (SPEC §5.5) that scopes
// every distributed embedding job this process enqueues or executes, reading it
// from — and seeding it into — the corpus's own metadata store.
//
// It replaces the root path the coordinator used to stamp onto jobs (#708). A
// path is the wrong key three times over: it is not stable (moving or
// re-mounting the corpus renames it), it is not unique for an object-store
// corpus (S3FS ignores the local root, so two buckets launched from one
// directory shared an identity — the same defect #737 fixed for the instance
// name), and it is not safe to publish into a queue several corpora may share.
// The persisted digest is all three.
//
// The key derivation is identity's, not a second implementation of it, so the
// corpus id and the MCP instance name are derived from exactly the same notion
// of "which corpus is this" and no credential can reach either.
func resolveCorpusID(ctx context.Context, cfg config.Config, st model.Store) (string, error) {
	settings, ok := st.(identity.SettingsStore)
	if !ok {
		return "", fmt.Errorf("store %T cannot persist %s; a shared broker cannot route this corpus's jobs safely",
			st, identity.CorpusIDSettingKey)
	}
	corpusID, err := identity.ResolveCorpusID(ctx, settings, corpusIdentityKey(cfg))
	if err != nil {
		return "", fmt.Errorf("resolve corpus id: %w", err)
	}
	return corpusID, nil
}

// corpusIdentityKey returns the canonical key this corpus is identified by,
// mirroring resolveServerName's source branch so one deployment can never be two
// identities depending on which of the two asked.
func corpusIdentityKey(cfg config.Config) string {
	if sourceIsRemote(cfg) {
		return identity.CorpusKeyForS3(cfg.Source.S3Bucket, cfg.Source.S3Prefix, cfg.Source.S3Endpoint)
	}
	abs, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		abs = cfg.RootDir
	}
	return identity.CorpusKey(abs)
}

// runCoordinatorLoop enqueues pending chunks on a fixed interval until ctx is
// cancelled. Re-enqueuing is safe: already-embedded chunks are no longer pending,
// and a duplicate job is idempotent at the embed layer (SPEC §8.7.3).
//
// It delegates the loop + stall detection to embedqueue.RunCoordinator (issue #435
// C4). A transient enqueue error stays best-effort (logged + non-blocking errCh
// send) so a blip never stalls the loop, but a SUSTAINED failure (StallThreshold
// consecutive errors) escalates to a durable, non-droppable signal — a
// context-aware BLOCKING errCh send — so a stalled coordinator surfaces as an
// embed_error event instead of a silently-idle daemon.
func runCoordinatorLoop(ctx context.Context, coord *embedqueue.Coordinator, errCh chan<- error, logger *log.Logger) {
	embedqueue.RunCoordinator(ctx, coord, embedqueue.CoordinatorLoopOptions{
		OnError: func(err error) {
			if logger != nil {
				logger.Printf("distributed embedding: enqueue pending: %v", err)
			}
			// Non-blocking: a transient enqueue error must not stall the loop if
			// errCh is momentarily full.
			select {
			case errCh <- fmt.Errorf("distributed embedding: enqueue pending: %w", err):
			default:
			}
		},
		OnStall: func(consecutive int, err error) {
			if logger != nil {
				logger.Printf("distributed embedding: enqueue STALLED after %d consecutive failures: %v", consecutive, err)
			}
			// Durable escalation: block until the signal is consumed (or shutdown)
			// so a sustained stall is never dropped by a full channel.
			select {
			case errCh <- fmt.Errorf("distributed embedding: enqueue stalled after %d consecutive failures: %w", consecutive, err):
			case <-ctx.Done():
			}
		},
	})
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
	embeddedGuard *embedqueue.EmbeddedGuard,
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	logger *log.Logger,
	maxFileBytes int64,
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
		BatchSize:    distributedEmbedBatchSize,
		Logger:       logger,
		// Same bound as the in-process loop: the distributed worker runs the same
		// media read, so it must not read past the operator's cap either (#830).
		MaxFileBytes: maxFileBytes,
		OnIndexedChunk: func(label uint64, metadata model.ChunkMetadata) {
			// Refreshing metadata is idempotent, so it runs on every (re-)embed.
			if ret != nil {
				ret.SetChunkMetadataForIndex(kind, label, metadata.ToSearchHit())
			}
			// The count is NOT idempotent, so gate it to a chunk's first successful
			// embed — a redelivered/retried job re-fires this hook (issue #435 C2).
			if indexingState != nil && embeddedGuard.First(kind, label) {
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
	embeddedGuard *embedqueue.EmbeddedGuard,
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	logger *log.Logger,
	maxFileBytes int64,
) map[string]embedqueue.Embedder {
	embedders := make(map[string]embedqueue.Embedder)
	if textIndex != nil {
		embedders["text"] = newEmbedStep(chunkSource, textIndex, embedder, ret, indexingState, embeddedGuard, textModel, codeModel, rootDir, corpusFS, logger, maxFileBytes, "text")
	}
	if codeIndex != nil {
		embedders["code"] = newEmbedStep(chunkSource, codeIndex, embedder, ret, indexingState, embeddedGuard, textModel, codeModel, rootDir, corpusFS, logger, maxFileBytes, "code")
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
