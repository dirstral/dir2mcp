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
	b.pending = append(b.pending, &memJob{job: job})
	return nil
}

// Lease reclaims any expired in-flight jobs, then claims the first pending job
// whose notBefore has passed.
func (b *MemBroker) Lease(_ context.Context, visibility time.Duration) (Lease, error) {
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.reclaimExpiredLocked(now)

	idx := -1
	for i, mj := range b.pending {
		if !mj.notBefore.After(now) {
			idx = i
			break
		}
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
	return Lease{Job: mj.job, Token: mj.token, Deadline: mj.deadline}, nil
}

// reclaimExpiredLocked moves in-flight jobs whose lease deadline has passed back
// to pending so a crashed/abandoned worker cannot strand a chunk (SPEC §8.7.3
// lease expiry). Caller holds b.mu.
func (b *MemBroker) reclaimExpiredLocked(now time.Time) {
	for token, mj := range b.inflight {
		if now.After(mj.deadline) {
			delete(b.inflight, token)
			mj.token = ""
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
