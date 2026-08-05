package embedqueue

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
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

// StatusWriter records a TERMINAL per-chunk embedding failure (SPEC §5.3
// embedding_status=error, §7.7 per-document/per-chunk errors). The metadata
// store satisfies it via MarkFailedWithCategory, the same entry point the
// in-process embedding loop uses, so a distributed failure lands in `dir2mcp
// status` / `doctor` in the exact shape an in-process one does.
//
// It exists because dead-lettering a job is not, by itself, a terminal outcome
// for the CHUNK: the broker forgets the job, the chunk stays `pending`, and the
// coordinator's next tick mints a fresh job with a fresh retry budget. Recording
// the failure against the chunk is what stops that loop (#709).
type StatusWriter interface {
	MarkFailedWithCategory(ctx context.Context, labels []uint64, category, reason string) error
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

	// Status records terminal per-chunk failures so a job that can never succeed
	// stops being re-created (#709). Optional: with no StatusWriter the worker
	// still rejects what it must, but a permanently-failing chunk stays `pending`
	// and the coordinator keeps re-enqueuing it, so production wiring MUST set it.
	Status StatusWriter

	// CorpusID is the stable corpus identity (SPEC §5.5) THIS worker serves — the
	// same value the coordinator for that corpus stamps on its jobs. A job naming
	// any other corpus is rejected without being executed and without touching
	// this worker's store.
	//
	// Empty means "unbound": the worker accepts every job it is handed, which is
	// only safe on a broker it does not share with another corpus. Production
	// wiring always sets it, resolved from the shared metadata store the job's
	// chunk would be read from, so a worker's corpus binding and its data plane
	// can never disagree (#708).
	CorpusID string

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
		lease, err := cfg.lease(ctx)
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
		lease, err := cfg.lease(ctx)
		if err != nil {
			// ErrNoJob (queue drained) or a transient lease error: embed what we
			// already hold; the next Run cycle re-leases / backs off.
			break
		}
		leases = append(leases, lease)
	}
	return leases
}

// lease claims one job, restricted to this worker's corpus when both the worker
// is bound to one and the broker can filter (SPEC §8.7.2). Claiming another
// corpus's job only to reject it is not merely wasteful: every rejected delivery
// consumes a redelivery attempt that belonged to the corpus that owns the job,
// so a mixed queue drained by the wrong worker dead-letters healthy work (#708).
// Brokers without the capability fall back to an unfiltered claim, where the
// per-job corpus check in prepare is the guard.
func (cfg Config) lease(ctx context.Context) (Lease, error) {
	corpusID := strings.TrimSpace(cfg.CorpusID)
	if scoped, ok := cfg.Broker.(CorpusScopedBroker); ok && corpusID != "" {
		return scoped.LeaseForCorpus(ctx, corpusID, cfg.leaseDuration())
	}
	return cfg.Broker.Lease(ctx, cfg.leaseDuration())
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
// be embedded. On ok=true the returned preparedJob carries the AUTHORITATIVE
// task and the embedder for the axis that task currently belongs to.
//
// The three gates run in this order, and the order is load-bearing:
//
//  1. corpus binding — is this job even ours? A foreign job is rejected before
//     anything reads or writes this worker's store (#708);
//  2. job-level identity — is the job well-formed, in our vector space, and is
//     its axis one we serve (SPEC §8.7.3);
//  3. payload currency — does the job still describe the chunk as it exists
//     now, and route to the axis that chunk is on NOW (#710).
func (cfg Config) prepare(ctx context.Context, lease Lease) (preparedJob, bool) {
	if !cfg.servesCorpus(ctx, lease) {
		return preparedJob{}, false
	}
	if !cfg.acceptsJob(ctx, lease) {
		return preparedJob{}, false
	}
	return cfg.routeToCurrentAxis(ctx, lease)
}

// servesCorpus reports whether the leased job belongs to the corpus this worker
// serves (SPEC §8.7.2 corpus reference, §8.7.4 corpus isolation). A foreign job
// is Nacked back for the corpus's own worker.
//
// Two properties matter here. It runs FIRST, before the store is touched: a
// worker resolves chunk ids against ITS OWN metadata store, so executing corpus
// A's job would read corpus B's chunk of the same id, embed it, write it into
// B's Tier-C namespace and Ack A's job — A silently loses the work and B gets a
// vector nothing asked for. And it never records a chunk failure: the chunk this
// job names is not in this worker's store, so there is nothing here to fail.
func (cfg Config) servesCorpus(ctx context.Context, lease Lease) bool {
	want := strings.TrimSpace(cfg.CorpusID)
	if want == "" {
		// Unbound worker (single-corpus deployments and the in-process default).
		return true
	}
	if got := strings.TrimSpace(lease.Job.CorpusID); got != want {
		// The job's corpus id is logged; it is a digest by construction
		// (identity.CorpusID), so this cannot disclose another corpus's path or
		// bucket. Never log lease.Token — it is a lease credential (§16.1.1).
		cfg.logf("embedqueue: job for chunk %d belongs to corpus %q, this worker serves %q; returning it unexecuted",
			lease.Job.ChunkID, got, want)
		_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
		return false
	}
	return true
}

// acceptsJob runs the checks that depend only on the job payload: it is
// well-formed, it was enqueued in this worker's vector space, and its axis is
// one this worker can write. Each failure is permanent for THIS worker, so the
// job goes back for another worker to try — and, on the delivery that exhausts
// the broker's budget, is recorded against the chunk so it stops being re-made.
func (cfg Config) acceptsJob(ctx context.Context, lease Lease) bool {
	job := lease.Job
	if err := job.Validate(); err != nil {
		cfg.logf("embedqueue: invalid job for chunk %d: %v", job.ChunkID, err)
		cfg.rejectJob(ctx, lease, string(store.ErrorCategoryEmbeddingFailure), "invalid embedding job: "+err.Error())
		return false
	}

	// Per-job embed identity enforcement (SPEC §8.7.3 / §6.4): a worker whose
	// embed identity does not match the job MUST NOT write a vector.
	if strings.TrimSpace(job.EmbedIdentity) != strings.TrimSpace(cfg.EmbedIdentity) {
		cfg.logf("embedqueue: embed identity mismatch for chunk %d (job=%q worker=%q); rejecting",
			job.ChunkID, job.EmbedIdentity, cfg.EmbedIdentity)
		cfg.rejectJob(ctx, lease, string(store.ErrorCategoryEmbeddingFailure),
			"embed identity mismatch: job was enqueued for a different embedding space than any worker in the pool provides")
		return false
	}
	return true
}

// routeToCurrentAxis loads the authoritative chunk and routes the job by what
// that chunk is NOW, not by what the job said it was at enqueue time.
//
// A chunk_id survives an in-place re-ingest of the same (rep_id, ordinal) while
// the text, the hash, the index_kind and the embedding status are all rewritten
// under it. A job enqueued before such a rewrite therefore names a payload that
// no longer exists. Executing it through the job's stale axis would write a
// code-axis vector for what is now a text chunk AND mark the chunk embedded, so
// the correct text-axis job is never created and a wrong-axis vector stays
// searchable. Such a job is ACKED as superseded, not failed: nothing is broken,
// the chunk is simply still pending and the coordinator will enqueue its current
// form (#710).
func (cfg Config) routeToCurrentAxis(ctx context.Context, lease Lease) (preparedJob, bool) {
	job := lease.Job
	// A tombstoned/missing chunk is a safe skip (tombstone safety, §8.7.3 / §6.6):
	// Ack so the job is not redelivered for a chunk that no longer exists.
	task, hash, err := cfg.Fetcher.ChunkTaskByID(ctx, job.ChunkID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			cfg.logf("embedqueue: chunk %d not found / tombstoned; acking (no-op)", job.ChunkID)
			_ = cfg.Broker.Ack(ctx, lease.Token)
			return preparedJob{}, false
		}
		cfg.logf("embedqueue: fetch chunk %d: %v; redelivering", job.ChunkID, err)
		cfg.rejectJob(ctx, lease, string(store.ClassifyError(err)),
			"could not read chunk from the shared metadata store: "+err.Error())
		return preparedJob{}, false
	}

	if reason, superseded := supersededReason(job, task, hash); superseded {
		cfg.logf("embedqueue: job for chunk %d is superseded (%s); acking without embedding", job.ChunkID, reason)
		_ = cfg.Broker.Ack(ctx, lease.Token)
		return preparedJob{}, false
	}

	// The axis comes from the TASK, never from the job: the two agree by the time
	// we get here (supersededReason just proved it), and taking it from the task
	// is what makes that guarantee structural rather than a convention a later
	// edit could quietly drop.
	indexKind := normalizeIndexKind(task.IndexKind)
	emb, ok := cfg.Embedders[indexKind]
	if !ok {
		cfg.logf("embedqueue: no embedder for index_kind %q (chunk %d); rejecting", indexKind, job.ChunkID)
		cfg.rejectJob(ctx, lease, string(store.ErrorCategoryEmbeddingFailure),
			"no embedder configured for index_kind "+indexKind)
		return preparedJob{}, false
	}
	return preparedJob{lease: lease, task: task, indexKind: indexKind, embedder: emb}, true
}

// supersededReason reports whether a leased job still describes the chunk the
// store holds, returning a short human-readable reason when it does not.
//
// An EMPTY hash on either side means "unknown", never "mismatch": jobs enqueued
// by a build that predates payload identity are still sitting in durable SQLite
// queues, and a chunk may legitimately carry no hash. Treating unknown as a
// mismatch would ack every one of them unexecuted and stall the queue, so the
// comparison is deliberately one-sided — it can only prove staleness, never
// freshness.
func supersededReason(job Job, task model.ChunkTask, storeHash string) (string, bool) {
	jobKind, taskKind := normalizeIndexKind(job.IndexKind), normalizeIndexKind(task.IndexKind)
	if jobKind != taskKind {
		return "chunk is now index_kind " + taskKind + ", job names " + jobKind, true
	}
	jobHash := strings.TrimSpace(job.TextHash)
	if jobHash != "" && strings.TrimSpace(storeHash) != "" && jobHash != strings.TrimSpace(storeHash) {
		return "chunk text_hash changed since the job was enqueued", true
	}
	return "", false
}

// normalizeIndexKind folds an index kind to its canonical form; an unset kind is
// "text" (SPEC §6.1), which is how both the coordinator and the store treat it.
func normalizeIndexKind(kind string) string {
	if k := strings.ToLower(strings.TrimSpace(kind)); k != "" {
		return k
	}
	return "text"
}

// rejectJob returns a job this worker could not execute to the broker and, when
// this delivery is the last the broker will make, records the failure against
// the chunk (SPEC §8.7.3: dead-lettering is "surfaced as a per-chunk error").
//
// Recording it is what makes dead-lettering TERMINAL. Dead-lettering alone only
// ends the JOB: the chunk stays `embedding_status=pending`, the coordinator's
// next tick selects it again, and a brand-new job starts a brand-new retry
// budget — a configuration mismatch then burns provider quota and broker rows
// forever without ever converging (#709). A chunk in `error` leaves the pending
// set, so no new job is minted, and the failure appears in `dir2mcp status` /
// `doctor` with a category and a sample instead of only as a queue counter an
// operator has no command to read.
//
// A failure that DESERVES a retry still gets every one it was budgeted: this
// fires only on the final delivery, so a transient fault that clears within the
// retry budget is redelivered and succeeds normally, and an operator who fixes
// the underlying cause re-pends the chunk (`dir2mcp reindex`) to hand it back to
// the queue.
func (cfg Config) rejectJob(ctx context.Context, lease Lease, category, reason string) {
	if lease.Final() {
		cfg.failChunk(ctx, lease, category, reason)
	}
	_ = cfg.Broker.Nack(ctx, lease.Token, cfg.retryAfter())
}

// failChunk records the terminal per-chunk failure, or says loudly why it could
// not. A worker with no StatusWriter cannot break the re-enqueue loop, and a
// silent inability to do so is exactly the failure mode #709 describes, so it is
// logged rather than ignored.
func (cfg Config) failChunk(ctx context.Context, lease Lease, category, reason string) {
	chunkID := lease.Job.ChunkID
	if cfg.Status == nil {
		cfg.logf("embedqueue: chunk %d exhausted its %d delivery attempts (%s) but no status writer is configured; "+
			"it stays pending and will be re-enqueued", chunkID, lease.Attempts, category)
		return
	}
	if err := cfg.Status.MarkFailedWithCategory(ctx, []uint64{chunkID}, category, store.SanitizeReason(reason)); err != nil {
		cfg.logf("embedqueue: record terminal failure for chunk %d: %v", chunkID, err)
		return
	}
	cfg.logf("embedqueue: chunk %d dead-lettered after %d attempts (%s); recorded as a terminal embedding error",
		chunkID, lease.Attempts, category)
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
			// and then dead-letter, mirroring the in-process behavior. A TRANSIENT
			// failure deliberately leaves the chunk pending (#412) so it is retried,
			// which is also why the final delivery has to record it: otherwise a
			// fault that outlives the retry budget loops forever (#709).
			cfg.logf("embedqueue: embed chunk %d (%s): %v; redelivering",
				group[0].lease.Job.ChunkID, indexKind, err)
			cfg.rejectJob(ctx, group[0].lease, string(store.ClassifyError(err)), "embedding failed: "+err.Error())
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
		cfg.rejectJob(ctx, pj.lease, string(store.ClassifyError(err)), "embedding failed: "+err.Error())
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
