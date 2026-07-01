package embedqueue_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
)

const testIdentity = "mistral|mistral-embed||0|0|off"

// fakeFetcher returns a stored task by chunk_id, or model.ErrNotFound to model a
// tombstoned/missing chunk (SPEC §8.7.3 tombstone safety).
type fakeFetcher struct {
	mu      sync.Mutex
	tasks   map[uint64]model.ChunkTask
	missing map[uint64]bool
	calls   int
}

func (f *fakeFetcher) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.missing[chunkID] {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	t, ok := f.tasks[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return t, "hash-" + t.Text, nil
}

// fakeEmbedStep records every chunk it embeds. Idempotency is verified by
// counting how many times each chunk_id is written (must be >=1 but each write
// overwrites the same logical vector — there are no duplicate vectors because the
// real index upserts by chunk_id; here we record the call sequence).
type fakeEmbedStep struct {
	mu      sync.Mutex
	written []uint64
}

func (e *fakeEmbedStep) EmbedAndIndex(_ context.Context, _ string, tasks []model.ChunkTask) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range tasks {
		e.written = append(e.written, t.Metadata.ChunkID)
	}
	return len(tasks), nil
}

func (e *fakeEmbedStep) writes() []uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]uint64, len(e.written))
	copy(out, e.written)
	return out
}

// batchSizes records the length of the task slice passed to each EmbedAndIndex
// call, so a test can assert the worker batched (a single N-chunk call) rather
// than making N one-chunk calls (issue #435).
type batchRecordingEmbedStep struct {
	mu      sync.Mutex
	written []uint64
	batches []int
}

func (e *batchRecordingEmbedStep) EmbedAndIndex(_ context.Context, _ string, tasks []model.ChunkTask) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.batches = append(e.batches, len(tasks))
	for _, t := range tasks {
		e.written = append(e.written, t.Metadata.ChunkID)
	}
	return len(tasks), nil
}

func (e *batchRecordingEmbedStep) snapshot() (writes []uint64, batches []int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	writes = append(writes, e.written...)
	batches = append(batches, e.batches...)
	return writes, batches
}

// poisonEmbedStep fails any batch that contains poisonID (as a real provider
// rejects a whole request when one input is bad) but succeeds otherwise, so a
// test can assert per-chunk isolation: the poison chunk is redelivered/dead-
// lettered while its innocent batch-mates still land (issue #435).
type poisonEmbedStep struct {
	mu       sync.Mutex
	poisonID uint64
	written  []uint64
}

func (e *poisonEmbedStep) EmbedAndIndex(_ context.Context, _ string, tasks []model.ChunkTask) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range tasks {
		if t.Metadata.ChunkID == e.poisonID {
			return 0, errors.New("poison chunk: permanent embed failure")
		}
	}
	for _, t := range tasks {
		e.written = append(e.written, t.Metadata.ChunkID)
	}
	return len(tasks), nil
}

func (e *poisonEmbedStep) writes() []uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]uint64, len(e.written))
	copy(out, e.written)
	return out
}

func textTask(id uint64, text string) model.ChunkTask {
	return model.NewChunkTask(id, text, "text", model.ChunkMetadata{ChunkID: id, RelPath: "a.txt"})
}

func runWorkerUntil(t *testing.T, cfg embedqueue.Config, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		_ = embedqueue.Run(ctx, cfg)
		close(finished)
	}()
	deadline := time.After(3 * time.Second)
	for {
		if done() {
			cancel()
			<-finished
			return
		}
		select {
		case <-deadline:
			cancel()
			<-finished
			t.Fatal("worker did not reach the expected state within 3s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestWorker_LeaseEmbedAck pins the happy path (SPEC §8.7.1/§8.7.3): the worker
// leases a job, reads the authoritative task, embeds it, and acks — the vector
// lands and the queue drains.
func TestWorker_LeaseEmbedAck(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(3)
	fetch := &fakeFetcher{tasks: map[uint64]model.ChunkTask{5: textTask(5, "hello")}}
	step := &fakeEmbedStep{}

	if err := broker.Enqueue(ctx, embedqueue.Job{
		ChunkID: 5, IndexKind: "text", EmbedIdentity: testIdentity,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity,
		PollInterval:  2 * time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		st, _ := broker.Stats(ctx)
		return st.Pending == 0 && st.InFlight == 0 && len(step.writes()) >= 1
	})

	writes := step.writes()
	if len(writes) != 1 || writes[0] != 5 {
		t.Fatalf("writes = %v, want [5]", writes)
	}
}

// TestWorker_IdempotentRedelivery pins idempotency (SPEC §8.7.3): re-processing
// the same chunk_id is safe — the vector write is keyed by chunk_id so re-running
// does not create a duplicate vector. The broker dedups LIVE jobs, so a duplicate
// enqueue while one is still queued is collapsed; once the first delivery drains,
// re-enqueuing the same chunk (a later coordinator pass / at-least-once
// redelivery) is permitted and embeds chunk_id 8 again, idempotently.
func TestWorker_IdempotentRedelivery(t *testing.T) {
	broker := embedqueue.NewMemBroker(3)
	fetch := &fakeFetcher{tasks: map[uint64]model.ChunkTask{8: textTask(8, "dup")}}
	step := &fakeEmbedStep{}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity,
		PollInterval:  2 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { _ = embedqueue.Run(ctx, cfg); close(finished) }()

	enqueue := func() {
		if err := broker.Enqueue(context.Background(), embedqueue.Job{
			ChunkID: 8, IndexKind: "text", EmbedIdentity: testIdentity,
		}); err != nil {
			t.Errorf("Enqueue: %v", err)
		}
	}
	waitForWrites := func(n int) {
		deadline := time.After(3 * time.Second)
		for {
			st, _ := broker.Stats(context.Background())
			if st.Pending == 0 && st.InFlight == 0 && len(step.writes()) >= n {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("worker did not reach %d writes within 3s (have %d)", n, len(step.writes()))
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	// First delivery embeds chunk 8 once and drains.
	enqueue()
	waitForWrites(1)
	// Re-enqueue the SAME chunk after it drained — dedup permits this (the prior
	// job was acked). The embed write is keyed by chunk_id, so re-processing
	// produces no duplicate vector (the real index upserts).
	enqueue()
	waitForWrites(2)

	cancel()
	<-finished

	for _, id := range step.writes() {
		if id != 8 {
			t.Fatalf("write for unexpected chunk %d; every redelivery must key chunk_id 8", id)
		}
	}
}

// TestWorker_EmbedIdentityMismatchRejected pins per-job embed-identity
// enforcement (SPEC §8.7.3/§6.4): a worker whose embed identity does not match
// the job MUST NOT embed it — no vector is written, preserving the single-space
// invariant. The mismatched job is redelivered until it dead-letters.
func TestWorker_EmbedIdentityMismatchRejected(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(2)
	fetch := &fakeFetcher{tasks: map[uint64]model.ChunkTask{4: textTask(4, "x")}}
	step := &fakeEmbedStep{}

	if err := broker.Enqueue(ctx, embedqueue.Job{
		ChunkID: 4, IndexKind: "text",
		EmbedIdentity: "gemini|gemini-embedding-001||3072|0|off", // different space
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity, // worker is mistral
		PollInterval:  1 * time.Millisecond,
		RetryAfter:    1 * time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		st, _ := broker.Stats(ctx)
		return st.DeadLettered == 1
	})

	if got := step.writes(); len(got) != 0 {
		t.Fatalf("worker embedded a mismatched-identity job: writes=%v", got)
	}
}

// TestWorker_BatchesUpToBatchSize pins the #435 fix: with several jobs queued the
// worker leases and embeds them in a SINGLE EmbedAndIndex call (up to BatchSize)
// instead of one provider call per chunk. Every enqueued chunk is embedded exactly
// once and the recorded batch sizes show batching (a call carrying >1 chunk).
func TestWorker_BatchesUpToBatchSize(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(3)
	tasks := map[uint64]model.ChunkTask{}
	const n = 5
	for id := uint64(1); id <= n; id++ {
		tasks[id] = textTask(id, "chunk")
	}
	fetch := &fakeFetcher{tasks: tasks}
	step := &batchRecordingEmbedStep{}

	for id := uint64(1); id <= n; id++ {
		if err := broker.Enqueue(ctx, embedqueue.Job{
			ChunkID: id, IndexKind: "text", EmbedIdentity: testIdentity,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity,
		BatchSize:     n, // drain the whole queue in one lease batch
		PollInterval:  2 * time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		w, _ := step.snapshot()
		st, _ := broker.Stats(ctx)
		return st.Pending == 0 && st.InFlight == 0 && len(w) >= n
	})

	writes, batches := step.snapshot()
	got := append([]uint64(nil), writes...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []uint64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("writes = %v, want each of %v exactly once", writes, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("writes = %v, want each of %v exactly once", writes, want)
		}
	}
	// The whole point of #435: at least one EmbedAndIndex call carried more than
	// one chunk. The old one-chunk-per-lease worker would show all 1s.
	maxBatch := 0
	for _, b := range batches {
		if b > maxBatch {
			maxBatch = b
		}
	}
	if maxBatch < 2 {
		t.Fatalf("no batching: EmbedAndIndex batch sizes = %v, want at least one call with >1 chunk", batches)
	}
}

// TestWorker_BatchPoisonChunkIsolated pins that a batch failure preserves
// per-chunk isolation (issue #435): a single poison chunk that fails the whole
// batch is redelivered (and eventually dead-lettered) WITHOUT dragging its
// innocent batch-mates down — those still land exactly once.
func TestWorker_BatchPoisonChunkIsolated(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(2) // dead-letter the poison chunk after 2 tries
	tasks := map[uint64]model.ChunkTask{
		1: textTask(1, "good-a"),
		2: textTask(2, "poison"),
		3: textTask(3, "good-b"),
	}
	fetch := &fakeFetcher{tasks: tasks}
	step := &poisonEmbedStep{poisonID: 2}

	for id := uint64(1); id <= 3; id++ {
		if err := broker.Enqueue(ctx, embedqueue.Job{
			ChunkID: id, IndexKind: "text", EmbedIdentity: testIdentity,
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity,
		BatchSize:     3,
		PollInterval:  1 * time.Millisecond,
		RetryAfter:    1 * time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		st, _ := broker.Stats(ctx)
		return st.DeadLettered == 1 && st.Pending == 0 && st.InFlight == 0
	})

	writes := step.writes()
	seen := map[uint64]int{}
	for _, id := range writes {
		seen[id]++
	}
	if seen[2] != 0 {
		t.Fatalf("poison chunk 2 was embedded (%d times); it must never land: writes=%v", seen[2], writes)
	}
	if seen[1] == 0 || seen[3] == 0 {
		t.Fatalf("innocent batch-mates did not land after isolation: writes=%v", writes)
	}
}

// TestWorker_TombstonedChunkSkipped pins tombstone safety (SPEC §8.7.3/§6.6): a
// job for a chunk that no longer exists (tombstoned ⇒ ChunkTaskByID returns
// ErrNotFound) is acked as a no-op — never embedded, never redelivered forever.
func TestWorker_TombstonedChunkSkipped(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(3)
	fetch := &fakeFetcher{missing: map[uint64]bool{6: true}}
	step := &fakeEmbedStep{}

	if err := broker.Enqueue(ctx, embedqueue.Job{
		ChunkID: 6, IndexKind: "text", EmbedIdentity: testIdentity,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetch,
		Embedders:     map[string]embedqueue.Embedder{"text": step},
		EmbedIdentity: testIdentity,
		PollInterval:  1 * time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		st, _ := broker.Stats(ctx)
		return st.Pending == 0 && st.InFlight == 0 && st.DeadLettered == 0
	})

	if got := step.writes(); len(got) != 0 {
		t.Fatalf("worker embedded a tombstoned chunk: writes=%v", got)
	}
}
