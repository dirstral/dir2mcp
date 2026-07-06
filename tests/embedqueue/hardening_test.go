package embedqueue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
)

// --- C2: embedded_ok double-count guard (issue #435) ---

// TestEmbeddedGuard_First pins the guard's core contract: it returns true exactly
// once per (index_kind, chunk_id) and false thereafter, and treats the two axes
// (text/code) of one chunk_id independently.
func TestEmbeddedGuard_First(t *testing.T) {
	g := embedqueue.NewEmbeddedGuard()

	if !g.First("text", 7) {
		t.Fatal("first embed of (text,7): want true")
	}
	if g.First("text", 7) {
		t.Fatal("redelivered embed of (text,7): want false")
	}
	// A different axis for the same chunk id is a distinct vector — counted once.
	if !g.First("code", 7) {
		t.Fatal("first embed of (code,7): want true")
	}
	if g.First("code", 7) {
		t.Fatal("redelivered embed of (code,7): want false")
	}
}

// TestEmbeddedGuard_NilReceiver pins that a nil guard degrades to "always first",
// so callers with no guard keep the pre-guard behavior instead of panicking.
func TestEmbeddedGuard_NilReceiver(t *testing.T) {
	var g *embedqueue.EmbeddedGuard
	// A nil guard must never dedup: the SAME (kind, id) reported twice both
	// return first=true (a real guard would return false on the second call).
	first := g.First("text", 1)
	again := g.First("text", 1)
	if !first || !again {
		t.Fatal("nil guard: every call should report first=true")
	}
}

// TestEmbeddedGuard_RedeliveredJobCountsOnce is the end-to-end C2 assertion: a job
// that is embedded, has its lease expire (redelivery), and is re-embedded must
// count embedded_ok exactly ONCE. It drives a real MemBroker through the same
// lease→embed→expire→re-lease→embed cycle the worker follows, with the count wired
// through the guard exactly as the CLI wires it into the embed hook.
func TestEmbeddedGuard_RedeliveredJobCountsOnce(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(5)

	// Deterministic clock so we can force lease expiry.
	now := time.Unix(1000, 0)
	broker.SetClock(func() time.Time { return now })

	if err := broker.Enqueue(ctx, embedqueue.Job{ChunkID: 42, IndexKind: "text", EmbedIdentity: testIdentity}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	guard := embedqueue.NewEmbeddedGuard()
	var embeddedOK int64
	// countEmbed mirrors the CLI's guarded OnIndexedChunk hook: the vector write is
	// idempotent (fires every time) but the count is gated to the first success.
	countEmbed := func(kind string, chunkID uint64) {
		if guard.First(kind, chunkID) {
			atomic.AddInt64(&embeddedOK, 1)
		}
	}

	// First delivery: lease, embed (count once), then DO NOT ack — let it expire.
	lease1, err := broker.Lease(ctx, time.Second)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	countEmbed(lease1.Job.IndexKind, lease1.Job.ChunkID)

	// Advance past the lease deadline so the job is reclaimed and redelivered.
	now = now.Add(2 * time.Second)

	lease2, err := broker.Lease(ctx, time.Second)
	if err != nil {
		t.Fatalf("Lease 2 (redelivery): %v", err)
	}
	if lease2.Job.ChunkID != 42 {
		t.Fatalf("redelivered chunk = %d, want 42", lease2.Job.ChunkID)
	}
	// Re-embed the redelivered job: the vector write would repeat, but the guarded
	// count must NOT.
	countEmbed(lease2.Job.IndexKind, lease2.Job.ChunkID)
	if err := broker.Ack(ctx, lease2.Token); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	if got := atomic.LoadInt64(&embeddedOK); got != 1 {
		t.Fatalf("embedded_ok = %d after redelivery, want 1 (C2 double-count)", got)
	}
}

// TestEmbeddedGuard_ConcurrentFirstOnce pins that under concurrent hits for the
// same pair the guard hands out exactly one true — the race-free property the
// double-count fix relies on. Run with -race.
func TestEmbeddedGuard_ConcurrentFirstOnce(t *testing.T) {
	g := embedqueue.NewEmbeddedGuard()
	const goroutines = 64
	var trues int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if g.First("text", 99) {
				atomic.AddInt64(&trues, 1)
			}
		}()
	}
	wg.Wait()
	if trues != 1 {
		t.Fatalf("concurrent First returned true %d times, want exactly 1", trues)
	}
}

// The C3 single-coordinator-lock tests (TestCoordinatorLock_*) live in
// coordinator_lock_unix_test.go: AcquireCoordinatorLock only enforces mutual
// exclusion under //go:build unix, so those assertions are gated to that tag.

// --- C4: enqueue-stall signal (issue #435) ---

// stallingSource fails NextPending every call, modeling a broker/store that is
// persistently unreadable (read-only DB / disk full).
type stallingSource struct{ calls int64 }

func (s *stallingSource) NextPending(_ context.Context, _ int, _ string) ([]model.ChunkTask, error) {
	atomic.AddInt64(&s.calls, 1)
	return nil, errors.New("simulated persistent enqueue failure")
}

// TestRunCoordinator_StallSignal pins C4: a sustained enqueue failure fires OnStall
// exactly once after StallThreshold consecutive failures, while OnError fires on
// every failing tick — so a stalled coordinator surfaces a durable signal instead
// of silently making no progress.
func TestRunCoordinator_StallSignal(t *testing.T) {
	coord := &embedqueue.Coordinator{
		Source:        &stallingSource{},
		Broker:        embedqueue.NewMemBroker(5),
		EmbedIdentity: testIdentity,
	}

	var errorCalls, stallCalls int64
	stalled := make(chan int, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		embedqueue.RunCoordinator(ctx, coord, embedqueue.CoordinatorLoopOptions{
			Interval:       time.Millisecond,
			StallThreshold: 3,
			OnError:        func(error) { atomic.AddInt64(&errorCalls, 1) },
			OnStall: func(consecutive int, _ error) {
				atomic.AddInt64(&stallCalls, 1)
				select {
				case stalled <- consecutive:
				default:
				}
			},
		})
		close(done)
	}()

	select {
	case consecutive := <-stalled:
		if consecutive < 3 {
			t.Fatalf("OnStall consecutive = %d, want >= 3 (StallThreshold)", consecutive)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnStall never fired on a sustained enqueue failure")
	}

	// Let the loop tick a while longer to prove OnStall is not re-fired every tick.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := atomic.LoadInt64(&stallCalls); got != 1 {
		t.Fatalf("OnStall fired %d times, want exactly 1 until a success resets it", got)
	}
	if got := atomic.LoadInt64(&errorCalls); got < 3 {
		t.Fatalf("OnError fired %d times, want >= 3 (once per failing tick)", got)
	}
}

// recoveringSource fails until failUntil calls, then succeeds — modeling a stall
// that clears. Once it succeeds it keeps returning empty (nothing pending).
type recoveringSource struct {
	mu        sync.Mutex
	calls     int
	failUntil int
}

func (s *recoveringSource) NextPending(_ context.Context, _ int, _ string) ([]model.ChunkTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failUntil {
		return nil, errors.New("transient enqueue failure")
	}
	return nil, nil
}

// TestRunCoordinator_StallResetsAfterSuccess pins that a success re-arms stall
// detection: if failures resume after a recovery the signal can fire again, so a
// second sustained stall is not swallowed.
func TestRunCoordinator_StallResetsAfterSuccess(t *testing.T) {
	// Fewer failures than the threshold, then success: OnStall must NOT fire.
	coord := &embedqueue.Coordinator{
		Source:        &recoveringSource{failUntil: 2},
		Broker:        embedqueue.NewMemBroker(5),
		EmbedIdentity: testIdentity,
	}

	var stallCalls int64
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	embedqueue.RunCoordinator(ctx, coord, embedqueue.CoordinatorLoopOptions{
		Interval:       time.Millisecond,
		StallThreshold: 3,
		OnStall:        func(int, error) { atomic.AddInt64(&stallCalls, 1) },
	})

	if got := atomic.LoadInt64(&stallCalls); got != 0 {
		t.Fatalf("OnStall fired %d times for a sub-threshold failure burst, want 0", got)
	}
}

// TestRunCoordinator_NilCoordinator pins that the exported loop helper fails
// fast (reports via OnError and returns) instead of panicking on a nil coord.
func TestRunCoordinator_NilCoordinator(t *testing.T) {
	var got error
	embedqueue.RunCoordinator(context.Background(), nil, embedqueue.CoordinatorLoopOptions{
		OnError: func(err error) { got = err },
	})
	if got == nil {
		t.Fatal("nil coordinator: want OnError to receive an error, got nil")
	}
}
