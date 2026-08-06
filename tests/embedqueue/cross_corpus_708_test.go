package embedqueue_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
)

// #708: two corpora pointed at ONE broker collided. Jobs carried `corpus_id`,
// but nothing keyed on it: both brokers deduplicated enqueues by (chunk_id,
// index_kind) only, and a worker executed whatever it leased against its OWN
// store. Chunk ids are per-corpus SQLite rowids, so both corpora have a chunk 1
// and the two failures compound — one corpus's enqueue is swallowed, and the
// other corpus's worker embeds the wrong bytes and acks work it never did.
//
// These tests set up the real thing: two real stores, two real corpus ids, one
// real shared SQLite broker file, and the real worker loop.

// sharedBroker opens one SQLite broker file, the deployment shape #708 is about
// (SPEC §8.7.4: a shared queue on an NFS-reachable path).
func sharedBroker(t *testing.T, maxAttempts int) *embedqueue.SQLiteBroker {
	t.Helper()
	broker, err := embedqueue.NewSQLiteBroker(context.Background(),
		filepath.Join(t.TempDir(), "embed-queue.db"), maxAttempts)
	if err != nil {
		t.Fatalf("open shared broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	return broker
}

// TestSharedBroker_TwoCorporaWithTheSameChunkIDBothGetTheirJob pins the enqueue
// half of the collision. Corpus A and corpus B each independently allocate chunk
// id 1. Deduplicating on (chunk_id, index_kind) alone made B's enqueue a no-op
// because A already had a live job for "1/text", so B's chunk was never queued
// at all and simply never got embedded.
func TestSharedBroker_TwoCorporaWithTheSameChunkIDBothGetTheirJob(t *testing.T) {
	ctx := context.Background()
	broker := sharedBroker(t, 3)

	corpusA := newTestCorpus(t, "alpha")
	corpusB := newTestCorpus(t, "beta")
	idA := corpusA.addChunk(t, 1, "alpha chunk", "hash-alpha", "text")
	idB := corpusB.addChunk(t, 1, "beta chunk", "hash-beta", "text")
	if idA != idB {
		t.Fatalf("fixture precondition: the two corpora allocated different chunk ids (%d vs %d); "+
			"the collision this test is about needs them to be the same", idA, idB)
	}
	if corpusA.id == corpusB.id {
		t.Fatalf("two distinct corpora resolved to the same corpus id %q", corpusA.id)
	}

	for _, c := range []*testCorpus{corpusA, corpusB} {
		coord := &embedqueue.Coordinator{
			Source: c.store, Broker: broker, CorpusID: c.id,
			SourceKind: "local", EmbedIdentity: testIdentity,
		}
		n, err := coord.EnqueuePending(ctx, "")
		if err != nil {
			t.Fatalf("%s: EnqueuePending: %v", c.name, err)
		}
		if n != 1 {
			t.Fatalf("%s: enqueued %d jobs, want 1", c.name, n)
		}
	}

	stats, err := broker.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 2 {
		t.Fatalf("shared queue holds %d pending job(s), want 2 (one per corpus); "+
			"a corpus whose chunk id collides with another's has silently lost its job", stats.Pending)
	}

	// And each queued job must be claimable BY ITS OWN corpus: one job each,
	// naming that corpus, with no second job left over for either.
	for _, c := range []*testCorpus{corpusA, corpusB} {
		lease, err := broker.LeaseForCorpus(ctx, c.id, time.Minute)
		if err != nil {
			t.Fatalf("%s: LeaseForCorpus: %v", c.name, err)
		}
		if lease.Job.CorpusID != c.id {
			t.Fatalf("%s: leased a job for corpus %q", c.name, lease.Job.CorpusID)
		}
		if _, err := broker.LeaseForCorpus(ctx, c.id, time.Minute); !errors.Is(err, embedqueue.ErrNoJob) {
			t.Fatalf("%s: a second job was claimable for this corpus, want ErrNoJob, got %v", c.name, err)
		}
	}
}

// TestSharedBroker_WorkerNeverExecutesAnotherCorpusJob pins the execution half,
// end to end through the real worker loop.
//
// Only corpus B runs a worker. Before the fix, that worker leased corpus A's job
// (nothing scoped the queue and nothing checked the binding), resolved chunk 1
// against ITS OWN store, embedded corpus B's bytes, wrote them under A's job and
// acked it. A lost the work while the queue reported it done; B got a vector for
// a chunk nobody asked to embed on A's behalf.
func TestSharedBroker_WorkerNeverExecutesAnotherCorpusJob(t *testing.T) {
	ctx := context.Background()
	broker := sharedBroker(t, 3)

	corpusA := newTestCorpus(t, "alpha")
	corpusB := newTestCorpus(t, "beta")
	chunkA := corpusA.addChunk(t, 1, "alpha chunk", "hash-alpha", "text")
	chunkB := corpusB.addChunk(t, 1, "beta chunk", "hash-beta", "text")

	// Only corpus A enqueues. B's worker must not touch that job at all.
	coordA := &embedqueue.Coordinator{
		Source: corpusA.store, Broker: broker, CorpusID: corpusA.id,
		SourceKind: "local", EmbedIdentity: testIdentity,
	}
	if _, err := coordA.EnqueuePending(ctx, ""); err != nil {
		t.Fatalf("alpha: EnqueuePending: %v", err)
	}

	embedderB := &recordingEmbedder{corpus: corpusB}
	workerB := embedqueue.Config{
		Broker:        broker,
		Fetcher:       corpusB.store,
		Embedders:     map[string]embedqueue.Embedder{"text": embedderB},
		Status:        corpusB.store,
		CorpusID:      corpusB.id,
		EmbedIdentity: testIdentity,
		PollInterval:  5 * time.Millisecond,
		RetryAfter:    time.Millisecond,
		Logger:        discardLogger(),
	}

	// Give B's worker a generous window to misbehave, then stop it.
	runWorkerFor(t, workerB, 400*time.Millisecond)

	if got := embedderB.texts(); len(got) != 0 {
		t.Fatalf("corpus beta's worker embedded %v; it must not execute another corpus's job", got)
	}
	if !corpusB.isPending(t, chunkB) {
		t.Fatalf("corpus beta's chunk %d left the pending set without any job for it", chunkB)
	}
	if !corpusA.isPending(t, chunkA) {
		t.Fatalf("corpus alpha's chunk %d was embedded by corpus beta's worker", chunkA)
	}

	// A's job must still be in the queue, intact and claimable, for A's own
	// worker: B must not have acked it away nor burned its retry budget.
	lease, err := broker.LeaseForCorpus(ctx, corpusA.id, time.Minute)
	if err != nil {
		t.Fatalf("corpus alpha's job is gone from the shared queue: %v", err)
	}
	if lease.Job.ChunkID != chunkA {
		t.Fatalf("leased chunk %d, want alpha's chunk %d", lease.Job.ChunkID, chunkA)
	}
	if lease.Attempts != 1 {
		t.Fatalf("alpha's job is on delivery attempt %d; corpus beta's worker consumed %d of alpha's retries",
			lease.Attempts, lease.Attempts-1)
	}
	if cats := corpusB.failureCategories(t); len(cats) != 0 {
		t.Fatalf("corpus beta recorded chunk failures %v while rejecting another corpus's job; "+
			"a foreign job names no chunk of this store and must never mark one failed", cats)
	}
}

// TestSharedBroker_EachCorpusWorkerEmbedsItsOwnBytes is the positive case: with
// both corpora coordinating and working against the one broker, each ends up
// with exactly its own chunk embedded, from its own store.
func TestSharedBroker_EachCorpusWorkerEmbedsItsOwnBytes(t *testing.T) {
	ctx := context.Background()
	broker := sharedBroker(t, 3)

	corpusA := newTestCorpus(t, "alpha")
	corpusB := newTestCorpus(t, "beta")
	corpusA.addChunk(t, 1, "alpha chunk", "hash-alpha", "text")
	corpusB.addChunk(t, 1, "beta chunk", "hash-beta", "text")

	embedders := map[string]*recordingEmbedder{}
	for _, c := range []*testCorpus{corpusA, corpusB} {
		coord := &embedqueue.Coordinator{
			Source: c.store, Broker: broker, CorpusID: c.id,
			SourceKind: "local", EmbedIdentity: testIdentity,
		}
		if _, err := coord.EnqueuePending(ctx, ""); err != nil {
			t.Fatalf("%s: EnqueuePending: %v", c.name, err)
		}
		embedders[c.name] = &recordingEmbedder{corpus: c}
	}

	// Both workers stop on the same condition — the shared queue fully drained —
	// rather than after a fixed window, so neither is cancelled between embedding
	// a chunk and acking it (which would leave a live lease behind and say
	// nothing about corpus routing).
	drained := func() bool {
		stats, err := broker.Stats(ctx)
		if err != nil {
			return false
		}
		return stats.Pending == 0 && stats.InFlight == 0
	}

	done := make(chan struct{}, 2)
	for _, c := range []*testCorpus{corpusA, corpusB} {
		cfg := embedqueue.Config{
			Broker:        broker,
			Fetcher:       c.store,
			Embedders:     map[string]embedqueue.Embedder{"text": embedders[c.name]},
			Status:        c.store,
			CorpusID:      c.id,
			EmbedIdentity: testIdentity,
			PollInterval:  5 * time.Millisecond,
			RetryAfter:    time.Millisecond,
			Logger:        discardLogger(),
		}
		go func() {
			if !runWorkerUntilOrTimeout(t, cfg, 5*time.Second, drained) {
				t.Errorf("%s: the shared queue never drained", c.name)
			}
			done <- struct{}{}
		}()
	}
	<-done
	<-done

	for _, c := range []*testCorpus{corpusA, corpusB} {
		got := embedders[c.name].texts()
		if len(got) != 1 || got[0] != c.name+" chunk" {
			t.Fatalf("%s embedded %v, want exactly its own %q", c.name, got, c.name+" chunk")
		}
	}
	stats, err := broker.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 0 || stats.InFlight != 0 || stats.DeadLettered != 0 {
		t.Fatalf("shared queue did not drain cleanly: %+v", stats)
	}
}
