package embedqueue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MemBroker is the default dependency-free, in-process Broker (SPEC §8.7.4). It
// is the degenerate single-node case: the coordinator and workers share one
// process and one queue with no external broker. It implements the full contract
// — at-least-once delivery, lease/visibility expiry, redelivery, and a
// dead-letter store — so it exercises the same worker code path a real broker
// would, which is exactly what the single-binary default and the test suite need.
//
// MemBroker is safe for concurrent Enqueue/Lease/Ack/Nack.
type MemBroker struct {
	mu          sync.Mutex
	pending     []*memJob // FIFO of claimable jobs (respecting notBefore)
	inflight    map[string]*memJob
	deadLetter  []*memJob
	maxAttempts int
	tokenSeq    atomic.Uint64
	now         func() time.Time // injectable clock for deterministic tests
}

type memJob struct {
	job       Job
	attempts  int
	token     string
	deadline  time.Time
	notBefore time.Time // job is not claimable before this (Nack retryAfter)
}

// Both built-in brokers carry the optional corpus-scoped claim path, so the
// default single-binary topology gets corpus isolation without an adapter.
var _ CorpusScopedBroker = (*MemBroker)(nil)

// NewMemBroker returns an in-process broker. maxAttempts bounds redelivery: a job
// Nacked after being delivered maxAttempts times is dead-lettered (SPEC §8.7.3).
// A non-positive maxAttempts defaults to 5.
func NewMemBroker(maxAttempts int) *MemBroker {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &MemBroker{
		inflight:    make(map[string]*memJob),
		maxAttempts: maxAttempts,
		now:         time.Now,
	}
}

// SetClock overrides the broker's clock for deterministic lease-expiry tests.
func (b *MemBroker) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now != nil {
		b.now = now
	}
}

// Enqueue appends a job to the pending FIFO.
func (b *MemBroker) Enqueue(_ context.Context, job Job) error {
	if err := job.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Dedup: skip when a LIVE (pending or in-flight) job for this corpus_id+
	// chunk_id+index_kind already exists, so re-enqueuing the same still-pending
	// head across coordinator ticks does not pile up duplicate jobs (SPEC §8.7.3).
	//
	// corpus_id is part of the key, not decoration: chunk ids are per-corpus
	// SQLite rowids, so two corpora sharing one broker BOTH have a chunk 1. Keyed
	// on (chunk_id, index_kind) alone, the second corpus's job was silently
	// swallowed by the first corpus's live job and that chunk was never embedded
	// (#708).
	for _, mj := range b.pending {
		if sameJobKey(mj.job, job) {
			return nil
		}
	}
	for _, mj := range b.inflight {
		if sameJobKey(mj.job, job) {
			return nil
		}
	}
	b.pending = append(b.pending, &memJob{job: job})
	return nil
}

// sameJobKey reports whether two jobs name the same unit of work: the same
// vector axis of the same chunk of the SAME corpus (SPEC §8.7.2).
func sameJobKey(a, b Job) bool {
	return a.CorpusID == b.CorpusID && a.ChunkID == b.ChunkID && a.IndexKind == b.IndexKind
}

// Lease reclaims any expired in-flight jobs, then claims the first pending job
// whose notBefore has passed, from any corpus.
func (b *MemBroker) Lease(ctx context.Context, visibility time.Duration) (Lease, error) {
	return b.LeaseForCorpus(ctx, "", visibility)
}

// LeaseForCorpus claims the first claimable pending job belonging to corpusID
// (SPEC §8.7.2 corpus reference). An empty corpusID claims from any corpus,
// which is what Lease does.
func (b *MemBroker) LeaseForCorpus(_ context.Context, corpusID string, visibility time.Duration) (Lease, error) {
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.reclaimExpiredLocked(now)

	idx := -1
	for i, mj := range b.pending {
		if mj.notBefore.After(now) {
			continue
		}
		if corpusID != "" && mj.job.CorpusID != corpusID {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		return Lease{}, ErrNoJob
	}

	mj := b.pending[idx]
	b.pending = append(b.pending[:idx], b.pending[idx+1:]...)
	mj.attempts++
	mj.token = fmt.Sprintf("mem-%d", b.tokenSeq.Add(1))
	mj.deadline = now.Add(visibility)
	b.inflight[mj.token] = mj
	return Lease{
		Job:         mj.job,
		Token:       mj.token,
		Deadline:    mj.deadline,
		Attempts:    mj.attempts,
		MaxAttempts: b.maxAttempts,
	}, nil
}

// reclaimExpiredLocked moves in-flight jobs whose lease deadline has passed back
// to pending so a crashed/abandoned worker cannot strand a chunk (SPEC §8.7.3
// lease expiry). A job whose lease expires after it has already exhausted
// maxAttempts is dead-lettered instead of redelivered, mirroring Nack's gate, so
// a chunk that reliably kills/hangs the worker before it can Ack/Nack cannot be
// re-leased forever. Caller holds b.mu.
func (b *MemBroker) reclaimExpiredLocked(now time.Time) {
	for token, mj := range b.inflight {
		if now.After(mj.deadline) {
			delete(b.inflight, token)
			mj.token = ""
			if mj.attempts >= b.maxAttempts {
				b.deadLetter = append(b.deadLetter, mj)
				continue
			}
			b.pending = append(b.pending, mj)
		}
	}
}

// Ack removes a leased job. An unknown/expired token is a no-op (it may have been
// reclaimed and redelivered).
func (b *MemBroker) Ack(_ context.Context, token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inflight, token)
	return nil
}

// Nack returns a leased job for redelivery, or dead-letters it once it has
// exhausted maxAttempts (SPEC §8.7.3). An unknown/expired token is a no-op.
func (b *MemBroker) Nack(_ context.Context, token string, retryAfter time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	mj, ok := b.inflight[token]
	if !ok {
		return nil
	}
	delete(b.inflight, token)
	mj.token = ""
	if mj.attempts >= b.maxAttempts {
		b.deadLetter = append(b.deadLetter, mj)
		return nil
	}
	if retryAfter > 0 {
		mj.notBefore = b.now().Add(retryAfter)
	}
	b.pending = append(b.pending, mj)
	return nil
}

// Stats reports queue depth, reclaiming expired leases first so InFlight is
// accurate.
func (b *MemBroker) Stats(_ context.Context) (Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reclaimExpiredLocked(b.now())
	return Stats{
		Pending:      len(b.pending),
		InFlight:     len(b.inflight),
		DeadLettered: len(b.deadLetter),
	}, nil
}

// Close is a no-op for the in-process broker.
func (b *MemBroker) Close() error { return nil }
