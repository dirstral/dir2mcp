package embedqueue_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
)

func sampleJob(chunkID uint64) embedqueue.Job {
	return embedqueue.Job{
		CorpusID:      "corpus-1",
		Source:        "local",
		ChunkID:       chunkID,
		IndexKind:     "text",
		EmbedIdentity: "mistral|mistral-embed||0|0|off",
	}
}

// brokerFactory builds a fresh broker for the shared contract tests so the same
// assertions run against both the in-process and the SQLite default impls.
type brokerFactory struct {
	name string
	make func(t *testing.T) embedqueue.Broker
	// clock lets a test drive lease expiry deterministically; nil means the
	// broker uses the real clock.
	setClock func(b embedqueue.Broker, now func() time.Time)
}

func brokerFactories() []brokerFactory {
	return []brokerFactory{
		{
			name: "mem",
			make: func(t *testing.T) embedqueue.Broker { return embedqueue.NewMemBroker(3) },
			setClock: func(b embedqueue.Broker, now func() time.Time) {
				b.(*embedqueue.MemBroker).SetClock(now)
			},
		},
		{
			name: "sqlite",
			make: func(t *testing.T) embedqueue.Broker {
				path := filepath.Join(t.TempDir(), "queue.db")
				b, err := embedqueue.NewSQLiteBroker(context.Background(), path, 3)
				if err != nil {
					t.Fatalf("NewSQLiteBroker: %v", err)
				}
				t.Cleanup(func() { _ = b.Close() })
				return b
			},
			setClock: func(b embedqueue.Broker, now func() time.Time) {
				b.(*embedqueue.SQLiteBroker).SetClock(now)
			},
		},
	}
}

// TestBroker_EnqueueLeaseAck pins the happy path (SPEC §8.7.3): a job is enqueued,
// leased exactly once, acked, and then the queue is empty.
func TestBroker_EnqueueLeaseAck(t *testing.T) {
	for _, bf := range brokerFactories() {
		t.Run(bf.name, func(t *testing.T) {
			ctx := context.Background()
			b := bf.make(t)

			if err := b.Enqueue(ctx, sampleJob(7)); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			lease, err := b.Lease(ctx, time.Minute)
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}
			if lease.Job.ChunkID != 7 {
				t.Fatalf("leased chunk = %d, want 7", lease.Job.ChunkID)
			}

			// Second lease before ack must find nothing (job is in-flight/hidden).
			if _, err := b.Lease(ctx, time.Minute); err != embedqueue.ErrNoJob {
				t.Fatalf("second Lease err = %v, want ErrNoJob", err)
			}
			if err := b.Ack(ctx, lease.Token); err != nil {
				t.Fatalf("Ack: %v", err)
			}
			st, err := b.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if st.Pending != 0 || st.InFlight != 0 {
				t.Fatalf("after ack stats = %+v, want empty", st)
			}
		})
	}
}

// TestBroker_LeaseExpiryRedelivery pins lease expiry (SPEC §8.7.3): a leased job
// not acked before its visibility deadline becomes re-claimable, so a crashed
// worker cannot strand a chunk.
func TestBroker_LeaseExpiryRedelivery(t *testing.T) {
	for _, bf := range brokerFactories() {
		t.Run(bf.name, func(t *testing.T) {
			ctx := context.Background()
			b := bf.make(t)
			now := time.Unix(1_000, 0)
			bf.setClock(b, func() time.Time { return now })

			if err := b.Enqueue(ctx, sampleJob(9)); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			first, err := b.Lease(ctx, 10*time.Second)
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}

			// Before the deadline the job stays hidden.
			now = now.Add(5 * time.Second)
			if _, err := b.Lease(ctx, 10*time.Second); err != embedqueue.ErrNoJob {
				t.Fatalf("pre-expiry Lease err = %v, want ErrNoJob", err)
			}

			// After the deadline the abandoned lease is reclaimed and re-leasable.
			now = now.Add(10 * time.Second)
			second, err := b.Lease(ctx, 10*time.Second)
			if err != nil {
				t.Fatalf("post-expiry Lease: %v", err)
			}
			if second.Job.ChunkID != 9 {
				t.Fatalf("reclaimed chunk = %d, want 9", second.Job.ChunkID)
			}
			if second.Token == first.Token {
				t.Fatalf("redelivery reused the same token %q", second.Token)
			}
		})
	}
}

// TestBroker_NackRedeliversThenDeadLetters pins redelivery + dead-lettering
// (SPEC §8.7.3): a job nacked past maxAttempts is dead-lettered, not requeued.
func TestBroker_NackRedeliversThenDeadLetters(t *testing.T) {
	for _, bf := range brokerFactories() {
		t.Run(bf.name, func(t *testing.T) {
			ctx := context.Background()
			b := bf.make(t) // maxAttempts = 3

			if err := b.Enqueue(ctx, sampleJob(3)); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			// Lease+Nack three times; the 3rd nack (attempts==maxAttempts) dead-letters.
			for i := 0; i < 3; i++ {
				lease, err := b.Lease(ctx, time.Minute)
				if err != nil {
					t.Fatalf("Lease attempt %d: %v", i+1, err)
				}
				if err := b.Nack(ctx, lease.Token, 0); err != nil {
					t.Fatalf("Nack attempt %d: %v", i+1, err)
				}
			}
			if _, err := b.Lease(ctx, time.Minute); err != embedqueue.ErrNoJob {
				t.Fatalf("after dead-letter Lease err = %v, want ErrNoJob", err)
			}
			st, err := b.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if st.DeadLettered != 1 {
				t.Fatalf("dead-lettered = %d, want 1 (stats=%+v)", st.DeadLettered, st)
			}
		})
	}
}

// TestBroker_ReclaimDeadLettersAfterMaxAttempts pins F1 (issue #433): the
// lease-reclaim path must respect maxAttempts. A chunk whose embedding reliably
// kills/hangs the worker dies before it can Ack/Nack, so each lease simply
// expires and is reclaimed. Reclaim redelivers only up to maxAttempts; once
// attempts are exhausted the next reclaim dead-letters the job instead of
// re-leasing it forever. Runs against both default broker impls.
func TestBroker_ReclaimDeadLettersAfterMaxAttempts(t *testing.T) {
	for _, bf := range brokerFactories() {
		t.Run(bf.name, func(t *testing.T) {
			ctx := context.Background()
			b := bf.make(t) // maxAttempts = 3
			now := time.Unix(2_000, 0)
			bf.setClock(b, func() time.Time { return now })

			if err := b.Enqueue(ctx, sampleJob(5)); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}

			// Lease the job maxAttempts times, letting each lease expire without an
			// Ack/Nack so the next Lease reclaims and redelivers it (the crash-loop).
			for i := 0; i < 3; i++ {
				lease, err := b.Lease(ctx, 10*time.Second)
				if err != nil {
					t.Fatalf("Lease attempt %d: %v", i+1, err)
				}
				if lease.Job.ChunkID != 5 {
					t.Fatalf("attempt %d leased chunk %d, want 5", i+1, lease.Job.ChunkID)
				}
				now = now.Add(11 * time.Second) // let the lease expire
			}

			// attempts == maxAttempts and the lease has expired: reclaim must
			// dead-letter the job, not redeliver it.
			if _, err := b.Lease(ctx, 10*time.Second); err != embedqueue.ErrNoJob {
				t.Fatalf("post-exhaustion Lease err = %v, want ErrNoJob (job must be dead-lettered)", err)
			}
			st, err := b.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if st.DeadLettered != 1 || st.Pending != 0 || st.InFlight != 0 {
				t.Fatalf("after reclaim stats = %+v, want DeadLettered=1 only", st)
			}
		})
	}
}

// TestSQLiteBroker_Durable confirms the SQLite broker persists enqueued jobs
// across a reopen (a coordinator restart does not lose the backlog).
func TestSQLiteBroker_Durable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")

	b1, err := embedqueue.NewSQLiteBroker(ctx, path, 5)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := b1.Enqueue(ctx, sampleJob(42)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	b2, err := embedqueue.NewSQLiteBroker(ctx, path, 5)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = b2.Close() }()
	lease, err := b2.Lease(ctx, time.Minute)
	if err != nil {
		t.Fatalf("Lease after reopen: %v", err)
	}
	if lease.Job.ChunkID != 42 {
		t.Fatalf("reopened lease chunk = %d, want 42", lease.Job.ChunkID)
	}
}

// TestBroker_EnqueueDedupsLiveJob pins that enqueuing the same chunk_id+index_kind
// while a job for it is still LIVE (pending/in-flight) is collapsed, so a
// coordinator re-enqueuing the same still-pending head across ticks does not pile
// up duplicate jobs (SPEC §8.7.3). A different chunk is not deduped, and once the
// live job drains (ack) the chunk may be enqueued again. Runs against both default
// broker impls.
func TestBroker_EnqueueDedupsLiveJob(t *testing.T) {
	for _, f := range brokerFactories() {
		t.Run(f.name, func(t *testing.T) {
			ctx := context.Background()
			b := f.make(t)
			defer func() { _ = b.Close() }()

			for i := 0; i < 3; i++ {
				if err := b.Enqueue(ctx, sampleJob(11)); err != nil {
					t.Fatalf("Enqueue %d: %v", i, err)
				}
			}
			st, err := b.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if st.Pending != 1 {
				t.Fatalf("Pending = %d, want 1 (duplicate live jobs must be deduped)", st.Pending)
			}

			// A different chunk is NOT deduped.
			if err := b.Enqueue(ctx, sampleJob(12)); err != nil {
				t.Fatalf("Enqueue other chunk: %v", err)
			}
			if st, _ = b.Stats(ctx); st.Pending != 2 {
				t.Fatalf("Pending = %d, want 2 (distinct chunk must enqueue)", st.Pending)
			}

			// Drain chunk 11, then re-enqueuing it is allowed (no live job remains).
			lease, err := b.Lease(ctx, time.Minute)
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}
			if err := b.Ack(ctx, lease.Token); err != nil {
				t.Fatalf("Ack: %v", err)
			}
			if err := b.Enqueue(ctx, sampleJob(lease.Job.ChunkID)); err != nil {
				t.Fatalf("re-enqueue after drain: %v", err)
			}
		})
	}
}

// TestSQLiteBroker_LeaseTokensUniqueAcrossInstances pins that lease tokens are
// globally unique: when two broker instances share one DB file and the same job
// row is re-leased after a lease expiry, the second lease gets a DIFFERENT token,
// so a stale Ack from the first instance cannot delete the second instance's
// current lease (SPEC §8.7.3).
func TestSQLiteBroker_LeaseTokensUniqueAcrossInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")
	base := time.Unix(1_700_000_000, 0)

	b1, err := embedqueue.NewSQLiteBroker(ctx, path, 5)
	if err != nil {
		t.Fatalf("open b1: %v", err)
	}
	defer func() { _ = b1.Close() }()
	b1.SetClock(func() time.Time { return base })

	b2, err := embedqueue.NewSQLiteBroker(ctx, path, 5)
	if err != nil {
		t.Fatalf("open b2: %v", err)
	}
	defer func() { _ = b2.Close() }()
	// b2's clock is far ahead, so it sees b1's lease as expired and reclaims it.
	b2.SetClock(func() time.Time { return base.Add(time.Hour) })

	if err := b1.Enqueue(ctx, sampleJob(7)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	l1, err := b1.Lease(ctx, time.Minute) // deadline = base + 1m
	if err != nil {
		t.Fatalf("b1 Lease: %v", err)
	}
	l2, err := b2.Lease(ctx, time.Minute) // base+1h > deadline -> reclaim + re-lease same row
	if err != nil {
		t.Fatalf("b2 Lease (reclaim): %v", err)
	}
	if l1.Token == l2.Token {
		t.Fatalf("lease tokens collided across instances for the same job row: %q", l1.Token)
	}
	// A stale Ack from b1 must NOT delete b2's now-current lease.
	if err := b1.Ack(ctx, l1.Token); err != nil {
		t.Fatalf("stale Ack: %v", err)
	}
	st, err := b2.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.InFlight != 1 {
		t.Fatalf("stale Ack removed the live lease: %+v", st)
	}
}
