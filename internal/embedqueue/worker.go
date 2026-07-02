package embedqueue

import (
	"context"
	"errors"
	"log"
	"sort"
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

	// BatchSize bounds how many jobs the worker leases and embeds per iteration
	// (SPEC §8.7, issue #435). The worker drains up to BatchSize claimable jobs,
	// groups them by index_kind, and embeds each group in a SINGLE provider call
	// via EmbedAndIndex — matching the in-process loop's batching instead of one
	// provider call per chunk. Non-positive defaults to 32 (the in-process
	// EmbeddingWorker default). A value of 1 restores the legacy one-at-a-time
	// behavior.
	BatchSize int

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

func (c Config) batchSize() int {
	if c.BatchSize <= 0 {
		return 32
	}
	return c.BatchSize
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

// preparedJob is a leased job that passed per-job validation/identity/kind checks
// and had its authoritative task loaded — ready to be embedded as part of a batch.
type preparedJob struct {
	lease     Lease
	task      model.ChunkTask
	indexKind string
	embedder  Embedder
}

// Run is the distributed embed-worker run-loop (SPEC §8.7.1). It leases jobs,
// reads the authoritative chunk via the shared store, enforces per-job embed
// identity, embeds via the shared in-process embedding path, and Acks on success
// or Nacks (redelivery / dead-letter) on transient failure. It blocks until ctx
// is cancelled and returns ctx.Err() then.
//
// Each iteration drains up to BatchSize claimable jobs and embeds them grouped by
// index_kind in a SINGLE provider call per group (issue #435), so the distributed
// path batches through the provider exactly like the in-process loop instead of
// making one embed call per chunk. Ack/Nack stays per-lease, so at-least-once
// delivery, per-job attempt tracking, and dead-lettering are unchanged.
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
		cfg.processBatch(ctx, cfg.drainBatch(ctx, lease))
	}
}

// drainBatch returns the initial lease plus up to BatchSize-1 additional
// currently-claimable jobs. It stops at the first empty queue or lease error —
// those are surfaced/backed-off on the next Run cycle — so a partial batch is
// embedded promptly rather than blocking for a full BatchSize.
func (cfg Config) drainBatch(ctx context.Context, first Lease) []Lease {
	leases := make([]Lease, 0, cfg.batchSize())
	leases = append(leases, first)
	for len(leases) < cfg.batchSize() {
		if ctx.Err() != nil {
			break
		}
		lease, err := cfg.Broker.Lease(ctx, cfg.leaseDuration())
		if err != nil {
			// ErrNoJob (queue drained) or a transient lease error: embed what we
			// already hold; the next Run cycle re-leases / backs off.
			break
		}
		leases = append(leases, lease)
	}
	return leases
}

// processBatch prepares every leased job (validate / identity / kind / fetch),
// terminally Acking or Nacking the ones that fail preparation, then embeds the
// survivors grouped by index_kind — one provider call per group.
func (cfg Config) processBatch(ctx context.Context, leases []Lease) {
	groups := make(map[string][]preparedJob)
	for _, lease := range leases {
		pj, ok := cfg.prepare(ctx, lease)
		if !ok {
			continue
		}
		groups[pj.indexKind] = append(groups[pj.indexKind], pj)
	}
	// Embed groups in a deterministic index_kind order (Go map iteration is
	// randomized). A stable order keeps the earliest-leased kinds from being
	// pushed to the back of an arbitrary shuffle, which would needlessly shrink
	// their remaining lease window and inflate redelivery/attempt counts.
	kinds := make([]string, 0, len(groups))
	for kind := range groups {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		cfg.embedGroup(ctx, kind, groups[kind])
	}
}

// prepare runs the per-job checks that gate embedding a single leased job. It
// returns ok=false (having already Acked/Nacked the lease) when the job must not
// be embedded: an invalid job, an embed-identity mismatch, or an unknown
// index_kind Nack for redelivery/dead-lettering; a tombstoned/missing chunk is
// Acked as a safe no-op (SPEC §8.7.3 / §6.6). On ok=true the returned preparedJob
// carries the authoritative task and its axis embedder.
func (cfg Config) prepare(ctx context.Context, lease Lease) (preparedJob, bool) {
	job := lease.Job
	if err := job.Validate(); err != nil {
		// Never log lease.Token — it is an opaque lease credential (§16.1.1).
		cfg.logf("embedqueue: invalid job for chunk %d: %v", job.ChunkID, err)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return preparedJob{}, false
	}

	// Per-job embed identity enforcement (SPEC §8.7.3 / §6.4): a worker whose
	// embed identity does not match the job MUST NOT write a vector. This is a
	// permanent mismatch for THIS worker, so Nack for redelivery to a matching
	// worker (or eventual dead-lettering) — never embed.
	if strings.TrimSpace(job.EmbedIdentity) != strings.TrimSpace(cfg.EmbedIdentity) {
		cfg.logf("embedqueue: embed identity mismatch for chunk %d (job=%q worker=%q); rejecting",
			job.ChunkID, job.EmbedIdentity, cfg.EmbedIdentity)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return preparedJob{}, false
	}

	indexKind := strings.ToLower(strings.TrimSpace(job.IndexKind))
	if indexKind == "" {
		indexKind = "text"
	}
	emb, ok := cfg.Embedders[indexKind]
	if !ok {
		cfg.logf("embedqueue: no embedder for index_kind %q (chunk %d); rejecting", indexKind, job.ChunkID)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return preparedJob{}, false
	}

	// Load the authoritative task from the shared store. A tombstoned/missing
	// chunk is a safe skip (tombstone safety, §8.7.3 / §6.6): Ack so the job is
	// not redelivered for a chunk that no longer exists.
	task, _, err := cfg.Fetcher.ChunkTaskByID(ctx, job.ChunkID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			cfg.logf("embedqueue: chunk %d not found / tombstoned; acking (no-op)", job.ChunkID)
			_ = cfg.Broker.Ack(ctx, lease.Token)
			return preparedJob{}, false
		}
		cfg.logf("embedqueue: fetch chunk %d: %v; redelivering", job.ChunkID, err)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return preparedJob{}, false
	}
	return preparedJob{lease: lease, task: task, indexKind: indexKind, embedder: emb}, true
}

// embedGroup embeds a same-index_kind group of prepared jobs in ONE provider call
// (EmbedAndIndex over the whole batch) and Acks every lease on success. On a batch
// failure it isolates: a lone job is Nacked for redelivery, but a multi-job batch
// is re-embedded one chunk at a time so a single poison chunk is redelivered /
// dead-lettered WITHOUT dragging its innocent batch-mates through the same retries
// (preserving per-chunk isolation, SPEC §8.7.3).
func (cfg Config) embedGroup(ctx context.Context, indexKind string, group []preparedJob) {
	if len(group) == 0 {
		return
	}
	// Drop any job whose lease already expired before we reached the provider.
	// Draining a full batch and fetching each authoritative task serially costs
	// time, so an early-leased job's window can be exhausted before EmbedAndIndex
	// is called (a real risk at BatchSize=32 with a slow shared store). Embedding
	// under a dead lease is wasted work: the follow-up Ack no-ops against a token
	// the broker has already invalidated, so the job is redelivered anyway. The
	// broker will re-lease it and embedding stays idempotent (keyed by chunk_id),
	// so skipping here is safe and avoids attempt inflation (SPEC §8.7.3;
	// Lease.Deadline).
	group = cfg.liveLeases(group)
	if len(group) == 0 {
		return
	}
	tasks := make([]model.ChunkTask, len(group))
	for i, pj := range group {
		tasks[i] = pj.task
	}
	if _, err := group[0].embedder.EmbedAndIndex(ctx, indexKind, tasks); err != nil {
		if len(group) == 1 {
			// EmbedAndIndex already records embedding_status=error for permanent
			// failures (SPEC §5.3); Nack lets the broker redeliver up to its limit
			// and then dead-letter, mirroring the in-process behavior.
			cfg.logf("embedqueue: embed chunk %d (%s): %v; redelivering",
				group[0].lease.Job.ChunkID, indexKind, err)
			_ = cfg.Broker.Nack(ctx, group[0].lease.Token, cfg.retryAfter())
			return
		}
		cfg.logf("embedqueue: batch embed of %d chunk(s) (%s) failed: %v; isolating per-chunk",
			len(group), indexKind, err)
		for _, pj := range group {
			cfg.embedOne(ctx, indexKind, pj)
		}
		return
	}
	for _, pj := range group {
		if err := cfg.Broker.Ack(ctx, pj.lease.Token); err != nil {
			cfg.logf("embedqueue: ack chunk %d: %v", pj.lease.Job.ChunkID, err)
		}
	}
}

// liveLeases returns the prepared jobs whose lease has not yet expired. An
// expired lease is left in place (not Acked/Nacked) so the broker redelivers it;
// embedding under it would only waste a provider call and a no-op Ack.
func (cfg Config) liveLeases(group []preparedJob) []preparedJob {
	now := time.Now()
	live := group[:0]
	for _, pj := range group {
		if !pj.lease.Deadline.IsZero() && !now.Before(pj.lease.Deadline) {
			cfg.logf("embedqueue: lease for chunk %d expired before embed; leaving for redelivery",
				pj.lease.Job.ChunkID)
			continue
		}
		live = append(live, pj)
	}
	return live
}

// embedOne embeds a single prepared job and Acks on success or Nacks on failure.
// It is the per-chunk isolation path taken after a batch embed fails, so a healthy
// chunk still lands (and its store status is corrected) while only the genuinely
// failing chunk is redelivered / dead-lettered.
func (cfg Config) embedOne(ctx context.Context, indexKind string, pj preparedJob) {
	if _, err := pj.embedder.EmbedAndIndex(ctx, indexKind, []model.ChunkTask{pj.task}); err != nil {
		cfg.logf("embedqueue: embed chunk %d (%s): %v; redelivering",
			pj.lease.Job.ChunkID, indexKind, err)
		_ = cfg.Broker.Nack(ctx, pj.lease.Token, cfg.retryAfter())
		return
	}
	if err := cfg.Broker.Ack(ctx, pj.lease.Token); err != nil {
		cfg.logf("embedqueue: ack chunk %d: %v", pj.lease.Job.ChunkID, err)
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
