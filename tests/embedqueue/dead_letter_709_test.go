package embedqueue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
)

// #709: the retry limit was not terminal. A job that could never succeed —
// embed identity mismatch, an axis no worker serves, a persistent store fault —
// was Nacked to exhaustion and dead-lettered, but the CHUNK stayed
// `embedding_status=pending`. The coordinator's next tick therefore selected it
// again and minted a fresh job with a fresh retry budget, forever: unbounded
// provider calls, an `embed_jobs` table that grows a dead row per cycle, and
// nothing in `status`/`doctor` ever saying why the corpus will not finish.
//
// The existing TestWorker_EmbedIdentityMismatchRejected stops at the FIRST dead
// letter and never runs a coordinator, so it cannot see the loop. These tests
// run both together and measure how many times the cycle repeats.

// runPairFor runs a coordinator and a worker against each other for one window,
// exactly as `up` wires them, and returns the queue state afterwards.
func runPairFor(t *testing.T, coord *embedqueue.Coordinator, worker embedqueue.Config, broker embedqueue.Broker, d time.Duration) embedqueue.Stats {
	t.Helper()
	coordDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		embedqueue.RunCoordinator(ctx, coord, embedqueue.CoordinatorLoopOptions{Interval: 5 * time.Millisecond})
		close(coordDone)
	}()
	runWorkerFor(t, worker, d)
	<-coordDone

	stats, err := broker.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return stats
}

// requireConverged runs the pair for TWO consecutive windows and requires the
// second to add nothing.
//
// The absolute dead-letter count is not the right measurement: a coordinator can
// legitimately read the pending set microseconds before the worker records the
// terminal failure, so ONE redundant job may still be minted. What distinguishes
// convergence from the #709 loop is whether the count keeps CLIMBING with
// wall-clock time. Two windows answer that; a single snapshot cannot.
func requireConverged(t *testing.T, coord *embedqueue.Coordinator, worker embedqueue.Config, broker embedqueue.Broker) {
	t.Helper()
	const window = 500 * time.Millisecond

	first := runPairFor(t, coord, worker, broker, window)
	second := runPairFor(t, coord, worker, broker, window)

	if second.DeadLettered != first.DeadLettered {
		t.Fatalf("dead letters grew from %d to %d over a second identical window: the chunk is being "+
			"re-enqueued with a fresh retry budget after every dead letter and will never converge",
			first.DeadLettered, second.DeadLettered)
	}
	if second.Pending != 0 || second.InFlight != 0 {
		t.Fatalf("queue has not settled: %+v", second)
	}
}

// TestDeadLetter_PermanentMismatchConvergesInsteadOfLooping runs a coordinator
// and a mismatched worker together, past the retry limit, and requires the
// system to SETTLE. The retry budget is 2 attempts against a 5ms coordinator
// tick and a 1ms redelivery delay, so each window below is worth dozens of
// re-enqueue cycles.
func TestDeadLetter_PermanentMismatchConvergesInsteadOfLooping(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	chunkID := corpus.addChunk(t, 1, "some text", "hash-1", "text")
	broker := sharedBroker(t, 2)

	coord := &embedqueue.Coordinator{
		Source: corpus.store, Broker: broker, CorpusID: corpus.id,
		SourceKind: "local", EmbedIdentity: testIdentity,
	}
	embedder := &recordingEmbedder{corpus: corpus}
	worker := embedqueue.Config{
		Broker:    broker,
		Fetcher:   corpus.store,
		Embedders: map[string]embedqueue.Embedder{"text": embedder},
		Status:    corpus.store,
		CorpusID:  corpus.id,
		// The worker's vector space is not the one the jobs were enqueued under:
		// a real, permanent, operator-visible misconfiguration.
		EmbedIdentity: "openai|text-embedding-3-small||1536|0|off",
		PollInterval:  time.Millisecond,
		RetryAfter:    time.Millisecond,
		Logger:        discardLogger(),
	}

	requireConverged(t, coord, worker, broker)

	if got := embedder.texts(); len(got) != 0 {
		t.Fatalf("a worker from another vector space embedded %v", got)
	}

	// The chunk itself must have left the pending set — that is what stops the
	// coordinator re-creating the job — and it must say why.
	if corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d is still pending after its job was dead-lettered; "+
			"the next coordinator tick will enqueue it again", chunkID)
	}
	cats := corpus.failureCategories(t)
	if total := totalCount(cats); total != 1 {
		t.Fatalf("status/doctor report %d failed chunk(s) %v, want exactly 1 with an actionable category", total, cats)
	}
}

// TestDeadLetter_UnservedAxisIsRecordedTerminally covers the second preparation
// failure #709 names: a job whose axis this pool has no embedder for. It is
// permanent in exactly the same way and must converge in exactly the same way.
func TestDeadLetter_UnservedAxisIsRecordedTerminally(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	chunkID := corpus.addChunk(t, 1, "func main() {}", "hash-1", "code")
	broker := sharedBroker(t, 2)

	coord := &embedqueue.Coordinator{
		Source: corpus.store, Broker: broker, CorpusID: corpus.id,
		SourceKind: "local", EmbedIdentity: testIdentity,
	}
	// Text-only pool: nothing can ever write the code axis.
	worker := embedqueue.Config{
		Broker:        broker,
		Fetcher:       corpus.store,
		Embedders:     map[string]embedqueue.Embedder{"text": &recordingEmbedder{corpus: corpus}},
		Status:        corpus.store,
		CorpusID:      corpus.id,
		EmbedIdentity: testIdentity,
		PollInterval:  time.Millisecond,
		RetryAfter:    time.Millisecond,
		Logger:        discardLogger(),
	}

	requireConverged(t, coord, worker, broker)

	if corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d is still pending, so the coordinator will keep re-enqueuing it", chunkID)
	}
	if total := totalCount(corpus.failureCategories(t)); total != 1 {
		t.Fatalf("status/doctor report %d failed chunk(s), want the one code chunk no worker can embed", total)
	}
}

// TestDeadLetter_TransientFailureStillGetsItsRetries is the other half of the
// contract and the reason the terminal write is gated on the LAST delivery
// rather than on any failure: a fault that clears within the retry budget must
// be redelivered and succeed, and must never leave a failure behind.
func TestDeadLetter_TransientFailureStillGetsItsRetries(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	chunkID := corpus.addChunk(t, 1, "some text", "hash-1", "text")
	broker := sharedBroker(t, 4)

	coord := &embedqueue.Coordinator{
		Source: corpus.store, Broker: broker, CorpusID: corpus.id,
		SourceKind: "local", EmbedIdentity: testIdentity,
	}
	if _, err := coord.EnqueuePending(context.Background(), ""); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}

	var attempts int
	embedder := &recordingEmbedder{corpus: corpus}
	embedder.failNext = func([]model.ChunkTask) error {
		attempts++
		if attempts <= 2 {
			return errors.New("connection reset by peer")
		}
		return nil
	}
	worker := embedqueue.Config{
		Broker:        broker,
		Fetcher:       corpus.store,
		Embedders:     map[string]embedqueue.Embedder{"text": embedder},
		Status:        corpus.store,
		CorpusID:      corpus.id,
		EmbedIdentity: testIdentity,
		PollInterval:  time.Millisecond,
		RetryAfter:    time.Millisecond,
		Logger:        discardLogger(),
	}

	if !runWorkerUntilOrTimeout(t, worker, 3*time.Second, func() bool {
		return len(embedder.texts()) > 0
	}) {
		t.Fatal("a chunk that failed twice transiently was never retried to success")
	}
	if corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d is still pending after a successful embed", chunkID)
	}
	if cats := corpus.failureCategories(t); len(cats) != 0 {
		t.Fatalf("a chunk that recovered within its retry budget was recorded as failed: %v", cats)
	}
}

func totalCount(counts map[string]int64) int64 {
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}
