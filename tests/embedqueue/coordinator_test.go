package embedqueue_test

import (
	"context"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakePendingSource hands out pending tasks once, then reports empty — modeling
// the store transitioning chunks out of "pending" after they are embedded.
type fakePendingSource struct {
	mu       sync.Mutex
	pending  []model.ChunkTask
	returned bool
}

func (s *fakePendingSource) NextPending(_ context.Context, _ int, _ string) ([]model.ChunkTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.returned {
		return nil, nil
	}
	s.returned = true
	return s.pending, nil
}

// TestCoordinator_EnqueuePending pins the coordinator role (SPEC §8.7.1/§8.7.2):
// it enqueues one job per pending chunk, bound to the corpus ref and the
// enqueue-time embed identity.
func TestCoordinator_EnqueuePending(t *testing.T) {
	ctx := context.Background()
	broker := embedqueue.NewMemBroker(3)
	src := &fakePendingSource{pending: []model.ChunkTask{
		model.NewChunkTask(1, "a", "text", model.ChunkMetadata{ChunkID: 1, RelPath: "a.txt"}),
		model.NewChunkTask(2, "b", "code", model.ChunkMetadata{ChunkID: 2, RelPath: "b.go"}),
	}}

	coord := &embedqueue.Coordinator{
		Source:        src,
		Broker:        broker,
		CorpusID:      "corpus-x",
		SourceKind:    "local",
		EmbedIdentity: testIdentity,
	}

	n, err := coord.EnqueuePending(ctx, "")
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if n != 2 {
		t.Fatalf("enqueued = %d, want 2", n)
	}

	st, _ := broker.Stats(ctx)
	if st.Pending != 2 {
		t.Fatalf("queue pending = %d, want 2", st.Pending)
	}

	// Drain and confirm jobs carry the corpus ref, chunk identity, and the
	// enqueue-time embed identity (SPEC §8.7.2).
	seen := map[uint64]embedqueue.Job{}
	for i := 0; i < 2; i++ {
		lease, err := broker.Lease(ctx, 0)
		if err != nil {
			t.Fatalf("Lease %d: %v", i, err)
		}
		seen[lease.Job.ChunkID] = lease.Job
	}
	if j, ok := seen[1]; !ok || j.IndexKind != "text" || j.CorpusID != "corpus-x" || j.EmbedIdentity != testIdentity {
		t.Fatalf("chunk 1 job wrong: %+v", j)
	}
	if j, ok := seen[2]; !ok || j.IndexKind != "code" {
		t.Fatalf("chunk 2 job wrong: %+v", j)
	}
}

// TestCoordinator_RequiresEmbedIdentity pins that the coordinator refuses to
// enqueue jobs with no embed identity (SPEC §8.7.2 — every job carries one).
func TestCoordinator_RequiresEmbedIdentity(t *testing.T) {
	coord := &embedqueue.Coordinator{
		Source: &fakePendingSource{},
		Broker: embedqueue.NewMemBroker(3),
	}
	if _, err := coord.EnqueuePending(context.Background(), ""); err == nil {
		t.Fatal("EnqueuePending with empty embed identity: want error, got nil")
	}
}
