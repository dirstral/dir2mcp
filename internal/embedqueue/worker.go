package embedqueue

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TaskFetcher loads the authoritative chunk task for a leased job from the shared
// store (SPEC §8.7.4 — chunk metadata/status lives in a store reachable by all
// workers). The metadata store satisfies it via ChunkTaskByID, which returns
// model.ErrNotFound for a missing or tombstoned chunk so the worker can skip it
// (tombstone safety, §8.7.3 / §6.6).
type TaskFetcher interface {
	ChunkTaskByID(ctx context.Context, chunkID uint64) (model.ChunkTask, string, error)
}

// Embedder is the embed→index→mark step the worker reuses. It is satisfied by
// *index.EmbeddingWorker.EmbedAndIndex, so the distributed worker shares the
// exact embed/media-load/index/mark logic of the in-process loop rather than
// duplicating it (idempotent, keyed by chunk_id — §8.7.3).
type Embedder interface {
	EmbedAndIndex(ctx context.Context, indexKind string, tasks []model.ChunkTask) (int, error)
}

// Config configures a distributed embed-worker run-loop (SPEC §8.7.1, the
// embed-worker / compute-plane role). All fields except the optional Logger and
// the tuning knobs are required.
type Config struct {
	// Broker is the queue the worker leases jobs from.
	Broker Broker
	// Fetcher loads the authoritative chunk task for a leased job.
	Fetcher TaskFetcher
	// Embedders maps an index kind ("text"/"code") to the embed→index→mark step
	// for that axis. A job whose index_kind has no embedder is dead-lettered
	// (misconfiguration, never a vector in the wrong space).
	Embedders map[string]Embedder

	// EmbedIdentity is THIS worker's configured embed identity (SPEC §8.1.4). A
	// job whose EmbedIdentity differs is rejected (Nacked for redelivery /
	// dead-lettering) rather than embedded, preserving the single-space invariant
	// across a heterogeneous pool (§8.7.3, §6.4).
	EmbedIdentity string

	// LeaseDuration is the visibility timeout for a leased job. Non-positive
	// defaults to 30s.
	LeaseDuration time.Duration
	// PollInterval is how long the loop waits after an empty queue before leasing
	// again. Non-positive defaults to 500ms.
	PollInterval time.Duration
	// RetryAfter delays redelivery of a Nacked (transiently failed) job.
	// Non-positive defaults to 2s.
	RetryAfter time.Duration

	// Logger is optional; nil routes to the standard log package. The worker logs
	// only job ids and error categories — never broker credentials, media bytes,
	// or presigned URLs.
	Logger *log.Logger
}

func (c Config) validate() error {
	if c.Broker == nil {
		return errors.New("embedqueue: worker requires a broker")
	}
	if c.Fetcher == nil {
		return errors.New("embedqueue: worker requires a task fetcher")
	}
	if len(c.Embedders) == 0 {
		return errors.New("embedqueue: worker requires at least one embedder")
	}
	if strings.TrimSpace(c.EmbedIdentity) == "" {
		return errors.New("embedqueue: worker requires an embed identity")
	}
	return nil
}

func (c Config) leaseDuration() time.Duration {
	if c.LeaseDuration <= 0 {
		return 30 * time.Second
	}
	return c.LeaseDuration
}

func (c Config) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return 500 * time.Millisecond
	}
	return c.PollInterval
}

func (c Config) retryAfter() time.Duration {
	if c.RetryAfter <= 0 {
		return 2 * time.Second
	}
	return c.RetryAfter
}

func (c Config) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run is the distributed embed-worker run-loop (SPEC §8.7.1). It leases jobs,
// reads the authoritative chunk via the shared store, enforces per-job embed
// identity, embeds via the shared in-process embedding path, and Acks on success
// or Nacks (redelivery / dead-letter) on transient failure. It blocks until ctx
// is cancelled and returns ctx.Err() then.
//
// It is exported as a reusable function because follow-up #249 wraps it in a CLI
// subcommand; #249 (the subcommand) is out of scope here.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		lease, err := cfg.Broker.Lease(ctx, cfg.leaseDuration())
		if errors.Is(err, ErrNoJob) {
			if waitErr := sleepCtx(ctx, cfg.pollInterval()); waitErr != nil {
				return waitErr
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cfg.logf("embedqueue: lease error: %v", err)
			if waitErr := sleepCtx(ctx, cfg.pollInterval()); waitErr != nil {
				return waitErr
			}
			continue
		}
		cfg.process(ctx, lease)
	}
}

// process executes one leased job and Acks/Nacks it. A permanent failure (embed
// identity mismatch, unknown index kind, tombstoned chunk) Acks/Nacks terminally
// so the job does not loop; a transient failure Nacks for redelivery.
func (cfg Config) process(ctx context.Context, lease Lease) {
	job := lease.Job
	if err := job.Validate(); err != nil {
		// Never log lease.Token — it is an opaque lease credential (§16.1.1).
		cfg.logf("embedqueue: invalid job for chunk %d: %v", job.ChunkID, err)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return
	}

	// Per-job embed identity enforcement (SPEC §8.7.3 / §6.4): a worker whose
	// embed identity does not match the job MUST NOT write a vector. This is a
	// permanent mismatch for THIS worker, so Nack for redelivery to a matching
	// worker (or eventual dead-lettering) — never embed.
	if strings.TrimSpace(job.EmbedIdentity) != strings.TrimSpace(cfg.EmbedIdentity) {
		cfg.logf("embedqueue: embed identity mismatch for chunk %d (job=%q worker=%q); rejecting",
			job.ChunkID, job.EmbedIdentity, cfg.EmbedIdentity)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return
	}

	indexKind := strings.ToLower(strings.TrimSpace(job.IndexKind))
	if indexKind == "" {
		indexKind = "text"
	}
	emb, ok := cfg.Embedders[indexKind]
	if !ok {
		cfg.logf("embedqueue: no embedder for index_kind %q (chunk %d); rejecting", indexKind, job.ChunkID)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return
	}

	// Load the authoritative task from the shared store. A tombstoned/missing
	// chunk is a safe skip (tombstone safety, §8.7.3 / §6.6): Ack so the job is
	// not redelivered for a chunk that no longer exists.
	task, _, err := cfg.Fetcher.ChunkTaskByID(ctx, job.ChunkID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			cfg.logf("embedqueue: chunk %d not found / tombstoned; acking (no-op)", job.ChunkID)
			_ = cfg.Broker.Ack(ctx, lease.Token)
			return
		}
		cfg.logf("embedqueue: fetch chunk %d: %v; redelivering", job.ChunkID, err)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return
	}

	if _, err := emb.EmbedAndIndex(ctx, indexKind, []model.ChunkTask{task}); err != nil {
		// EmbedAndIndex already records embedding_status=error for permanent
		// failures (SPEC §5.3); Nack lets the broker redeliver up to its limit and
		// then dead-letter, mirroring the in-process retry/dead-letter behavior.
		cfg.logf("embedqueue: embed chunk %d (%s): %v; redelivering", job.ChunkID, indexKind, err)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return
	}
	if err := cfg.Broker.Ack(ctx, lease.Token); err != nil {
		cfg.logf("embedqueue: ack chunk %d: %v", job.ChunkID, err)
	}
}

// sleepCtx waits for d or ctx cancellation, returning ctx.Err() on cancel.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
