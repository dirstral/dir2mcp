package embedqueue_test

import (
	"context"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
)

// #710: jobs carried no payload identity and workers threw away the one the
// store returned. A chunk_id survives an in-place re-ingest of the same
// (rep_id, ordinal) while text, text_hash, index_kind and embedding status are
// all rewritten under it, so a job enqueued before such a rewrite named bytes
// that no longer existed — and the worker routed the NEW task through the OLD
// job's axis and embedder, marking the chunk embedded on the wrong axis.
//
// These tests are deterministic: the chunk is rewritten between enqueue and
// lease, with no concurrency.

// enqueueOnce runs one coordinator pass over the corpus's pending head.
func enqueueOnce(t *testing.T, corpus *testCorpus, broker embedqueue.Broker) int {
	t.Helper()
	coord := &embedqueue.Coordinator{
		Source: corpus.store, Broker: broker, CorpusID: corpus.id,
		SourceKind: "local", EmbedIdentity: testIdentity,
	}
	n, err := coord.EnqueuePending(context.Background(), "")
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	return n
}

// twoAxisWorker builds a worker that serves both axes, so a mis-route lands in a
// real embedder rather than being caught by a missing one.
func twoAxisWorker(corpus *testCorpus, broker embedqueue.Broker, text, code *recordingEmbedder) embedqueue.Config {
	return embedqueue.Config{
		Broker:        broker,
		Fetcher:       corpus.store,
		Embedders:     map[string]embedqueue.Embedder{"text": text, "code": code},
		Status:        corpus.store,
		CorpusID:      corpus.id,
		EmbedIdentity: testIdentity,
		PollInterval:  time.Millisecond,
		RetryAfter:    time.Millisecond,
		Logger:        discardLogger(),
	}
}

// TestStaleJob_AxisChangeIsNotEmbeddedThroughTheOldAxis is the race from the
// issue, made deterministic: enqueue chunk N while it is `code`, re-ingest the
// same (rep_id, ordinal) as `text` (same chunk id, new hash, new axis, pending
// again), then let a worker lease the old job.
//
// The old job must not be executed. Executing it wrote a CODE vector for what is
// now a text chunk and marked the chunk embedded, so the correct text-axis job
// was never enqueued and a wrong-axis vector stayed searchable.
func TestStaleJob_AxisChangeIsNotEmbeddedThroughTheOldAxis(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	broker := sharedBroker(t, 3)

	chunkID := corpus.addChunk(t, 1, "func main() {}", "hash-code", "code")
	if n := enqueueOnce(t, corpus, broker); n != 1 {
		t.Fatalf("enqueued %d jobs, want 1", n)
	}
	requireQueuedJobCarriesHash(t, broker, corpus, "hash-code")

	// The in-place re-ingest: same representation and ordinal, so the store keeps
	// the chunk id and rewrites everything else.
	if got := corpus.addChunk(t, 1, "just prose now", "hash-text", "text"); got != chunkID {
		t.Fatalf("re-ingest allocated chunk %d, want the same id %d; the fixture is not "+
			"reproducing the in-place update this test is about", got, chunkID)
	}

	textEmbedder := &recordingEmbedder{corpus: corpus}
	codeEmbedder := &recordingEmbedder{corpus: corpus}
	runWorkerFor(t, twoAxisWorker(corpus, broker, textEmbedder, codeEmbedder), 300*time.Millisecond)

	if got := codeEmbedder.texts(); len(got) != 0 {
		t.Fatalf("the stale code-axis job embedded %v; the chunk is a TEXT chunk now and a code "+
			"vector for it would stay searchable on the wrong axis", got)
	}
	if got := textEmbedder.texts(); len(got) != 0 {
		t.Fatalf("the stale job was re-routed to the text axis and embedded %v; a superseded job "+
			"must be acked, leaving the coordinator to enqueue the chunk's current form", got)
	}
	if !corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d was marked embedded by a job that named its previous form", chunkID)
	}

	// The stale job is gone (acked, not retried) rather than lingering or being
	// dead-lettered: nothing was wrong with it, it was simply out of date.
	stats, err := broker.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 0 || stats.InFlight != 0 || stats.DeadLettered != 0 {
		t.Fatalf("superseded job was not acked away: %+v", stats)
	}

	// And the chunk converges: the next coordinator pass enqueues its CURRENT
	// form, which embeds on the axis it is actually on now.
	if n := enqueueOnce(t, corpus, broker); n != 1 {
		t.Fatalf("re-enqueued %d jobs for the updated chunk, want 1", n)
	}
	if !runWorkerUntilOrTimeout(t, twoAxisWorker(corpus, broker, textEmbedder, codeEmbedder),
		3*time.Second, func() bool { return len(textEmbedder.texts()) > 0 }) {
		t.Fatal("the chunk's current text-axis form was never embedded")
	}
	if got := codeEmbedder.texts(); len(got) != 0 {
		t.Fatalf("code axis embedded %v after the chunk became text", got)
	}
}

// requireQueuedJobCarriesHash inspects the head of the queue without consuming
// it: a job that names no payload gives the worker nothing to compare against,
// so every downstream assertion in this file would be vacuous.
func requireQueuedJobCarriesHash(t *testing.T, broker *embedqueue.SQLiteBroker, corpus *testCorpus, want string) {
	t.Helper()
	ctx := context.Background()
	lease, err := broker.LeaseForCorpus(ctx, corpus.id, time.Minute)
	if err != nil {
		t.Fatalf("inspect queued job: %v", err)
	}
	if lease.Job.TextHash != want {
		t.Fatalf("queued job carries text_hash %q, want the chunk's persisted %q; a job with no "+
			"payload identity cannot be recognised as superseded (SPEC §8.7.2)", lease.Job.TextHash, want)
	}
	if err := broker.Nack(ctx, lease.Token, 0); err != nil {
		t.Fatalf("return inspected job: %v", err)
	}
}

// TestStaleJob_ChangedTextOnTheSameAxisIsNotEmbedded covers the same-axis case:
// the chunk keeps index_kind=text but its bytes are replaced. The axis check
// alone would let this through, so it is the text_hash that has to catch it.
func TestStaleJob_ChangedTextOnTheSameAxisIsNotEmbedded(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	broker := sharedBroker(t, 3)

	chunkID := corpus.addChunk(t, 1, "first revision", "hash-v1", "text")
	if n := enqueueOnce(t, corpus, broker); n != 1 {
		t.Fatalf("enqueued %d jobs, want 1", n)
	}
	if got := corpus.addChunk(t, 1, "second revision", "hash-v2", "text"); got != chunkID {
		t.Fatalf("re-ingest allocated chunk %d, want %d", got, chunkID)
	}

	textEmbedder := &recordingEmbedder{corpus: corpus}
	codeEmbedder := &recordingEmbedder{corpus: corpus}
	runWorkerFor(t, twoAxisWorker(corpus, broker, textEmbedder, codeEmbedder), 300*time.Millisecond)

	if got := textEmbedder.texts(); len(got) != 0 {
		t.Fatalf("a job enqueued for text_hash hash-v1 embedded %v; a job whose payload has been "+
			"replaced must be acked as superseded, not executed", got)
	}
	if !corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d was marked embedded through a superseded job", chunkID)
	}
}

// TestStaleJob_UnchangedChunkStillEmbeds is the guard against over-rejection:
// the check must only fire when the payload actually changed. A job whose chunk
// is untouched embeds exactly as before.
func TestStaleJob_UnchangedChunkStillEmbeds(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	broker := sharedBroker(t, 3)

	chunkID := corpus.addChunk(t, 1, "unchanged text", "hash-v1", "text")
	if n := enqueueOnce(t, corpus, broker); n != 1 {
		t.Fatalf("enqueued %d jobs, want 1", n)
	}

	textEmbedder := &recordingEmbedder{corpus: corpus}
	codeEmbedder := &recordingEmbedder{corpus: corpus}
	if !runWorkerUntilOrTimeout(t, twoAxisWorker(corpus, broker, textEmbedder, codeEmbedder),
		3*time.Second, func() bool { return len(textEmbedder.texts()) > 0 }) {
		t.Fatal("an unchanged chunk was never embedded")
	}
	if got := textEmbedder.texts(); len(got) != 1 || got[0] != "unchanged text" {
		t.Fatalf("text axis embedded %v, want the chunk once", got)
	}
	if corpus.isPending(t, chunkID) {
		t.Fatalf("chunk %d is still pending after a successful embed", chunkID)
	}
}

// TestStaleJob_JobWithoutPayloadIdentityStillEmbeds pins the compatibility rule
// the comparison depends on: a durable SQLite queue can hold jobs written by a
// build that predated payload identity. An empty hash means UNKNOWN, so such a
// job must still execute — treating it as a mismatch would ack every queued job
// on upgrade and stall the corpus.
func TestStaleJob_JobWithoutPayloadIdentityStillEmbeds(t *testing.T) {
	corpus := newTestCorpus(t, "alpha")
	broker := sharedBroker(t, 3)

	chunkID := corpus.addChunk(t, 1, "legacy job text", "hash-v1", "text")
	if err := broker.Enqueue(context.Background(), embedqueue.Job{
		CorpusID:      corpus.id,
		Source:        "local",
		ChunkID:       chunkID,
		IndexKind:     "text",
		EmbedIdentity: testIdentity,
		// TextHash deliberately absent, as a pre-#710 job on disk would be.
	}); err != nil {
		t.Fatalf("enqueue legacy job: %v", err)
	}

	textEmbedder := &recordingEmbedder{corpus: corpus}
	codeEmbedder := &recordingEmbedder{corpus: corpus}
	if !runWorkerUntilOrTimeout(t, twoAxisWorker(corpus, broker, textEmbedder, codeEmbedder),
		3*time.Second, func() bool { return len(textEmbedder.texts()) > 0 }) {
		t.Fatal("a job carrying no payload identity was never executed; an unknown hash must not read as stale")
	}
}
