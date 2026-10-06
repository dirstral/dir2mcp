package embedqueue_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestBroker_DocumentJobDedupsPerRepresentation pins the document-ownership rule
// (SPEC §8.1.9 "Distributed workers": no two workers pool chunks of one
// representation concurrently) at the broker: while a document job for a
// representation is live (pending or in flight), a second document job for the
// SAME representation with a DIFFERENT first chunk id is dropped, on both shipped
// brokers. A per-chunk job keeps its per-chunk key, and another representation's
// document job still enqueues.
func TestBroker_DocumentJobDedupsPerRepresentation(t *testing.T) {
	ctx := context.Background()
	sqlBroker, err := embedqueue.NewSQLiteBroker(ctx, filepath.Join(testutil.TempDir(t), "queue.sqlite"), 3)
	if err != nil {
		t.Fatalf("NewSQLiteBroker: %v", err)
	}
	t.Cleanup(func() { _ = sqlBroker.Close() })
	for name, broker := range map[string]embedqueue.Broker{"mem": embedqueue.NewMemBroker(3), "sqlite": sqlBroker} {
		t.Run(name, func(t *testing.T) {
			// Job A for rep 7 goes in flight (leased) with chunks 5,6,7.
			if err := broker.Enqueue(ctx, documentJob(7, 5, 6, 7)); err != nil {
				t.Fatalf("enqueue A: %v", err)
			}
			if _, err := broker.Lease(ctx, time.Minute); err != nil {
				t.Fatalf("lease A: %v", err)
			}
			// Chunk 5 left pending meanwhile; the next coordinator pass builds job B
			// for the same representation with first chunk 6. It must be dropped.
			if err := broker.Enqueue(ctx, documentJob(7, 6, 7)); err != nil {
				t.Fatalf("enqueue B: %v", err)
			}
			st, _ := broker.Stats(ctx)
			if st.Pending != 0 || st.InFlight != 1 {
				t.Fatalf("second document job for the same representation must be dropped while one is live: %+v", st)
			}
			// Another representation, and a per-chunk job, are independent units.
			if err := broker.Enqueue(ctx, documentJob(8, 9)); err != nil {
				t.Fatalf("enqueue rep 8: %v", err)
			}
			if err := broker.Enqueue(ctx, embedqueue.Job{CorpusID: "c", Source: "local", ChunkID: 6, IndexKind: "text", EmbedIdentity: lcIdentity}); err != nil {
				t.Fatalf("enqueue per-chunk: %v", err)
			}
			st, _ = broker.Stats(ctx)
			if st.Pending != 2 {
				t.Fatalf("rep 8 document job and the per-chunk job must both enqueue: %+v", st)
			}
		})
	}
}

// TestWorker_CorruptDocumentJobRowIsNamedAsCorrupt pins the reason a worker
// records for a document job row whose chunk_ids column is unreadable: it is a
// corrupt row, not a per-chunk job from an old coordinator, so the reason must
// say "corrupt document job row" and never the coordinator-upgrade text. The row
// is written through the broker's own database handle, as corruption would be.
func TestWorker_CorruptDocumentJobRowIsNamedAsCorrupt(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(testutil.TempDir(t), "queue.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker, err := embedqueue.NewSQLiteBrokerWithDB(ctx, db, 1) // one delivery, then dead-letter
	if err != nil {
		t.Fatalf("NewSQLiteBrokerWithDB: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO embed_jobs(corpus_id, source, chunk_id, index_kind, text_hash, modality, rel_path,
  span_kind, span_page, span_start_ms, span_end_ms, embed_identity, rep_id, chunk_ids, state)
VALUES ('c', 'local', 5, 'text', '', 'text', '', '', 0, 0, 0, ?, 7, 'not-json', 'pending')`, lcIdentity); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}
	fetch := &fakeFetcher{tasks: map[uint64]model.ChunkTask{5: repTask(5, 7, "alpha")}}
	emb := &fakeEmbedStep{}
	status := &lcStatus{}
	cfg := embedqueue.Config{
		Broker: broker, Fetcher: fetch, Embedders: map[string]embedqueue.Embedder{"text": emb},
		Status: status, EmbedIdentity: lcIdentity, LateChunking: true,
		PollInterval: time.Millisecond, RetryAfter: time.Millisecond,
	}
	runWorkerUntil(t, cfg, func() bool {
		st, _ := broker.Stats(ctx)
		return st.DeadLettered == 1
	})
	if w := emb.writes(); len(w) != 0 {
		t.Fatalf("a corrupt row must not be embedded, wrote %v", w)
	}
	labels, reason := status.snapshot()
	if len(labels) != 1 || labels[0] != 5 {
		t.Fatalf("the row's chunk must be recorded failed: %v", labels)
	}
	if !strings.Contains(reason, "corrupt document job row") {
		t.Fatalf("reason must name the corrupt row: %q", reason)
	}
	if strings.Contains(reason, "upgrade the coordinator") {
		t.Fatalf("reason must not blame the coordinator for a corrupt row: %q", reason)
	}
}

// TestBroker_IncomingJobChoosesTheDedupKey pins the #951 review finding on the
// memory broker: the INCOMING job picks the key, as the SQLite probe does. A live
// per-chunk job for chunk 5 must not swallow a document job for representation
// 7 whose first chunk happens to be 5, and a live document job must not swallow
// an unrelated per-chunk job that shares its first chunk id. Both brokers must
// agree, or the memory broker defers document processing across ticks.
func TestBroker_IncomingJobChoosesTheDedupKey(t *testing.T) {
	ctx := context.Background()
	sqlBroker, err := embedqueue.NewSQLiteBroker(ctx, filepath.Join(testutil.TempDir(t), "queue.sqlite"), 3)
	if err != nil {
		t.Fatalf("NewSQLiteBroker: %v", err)
	}
	t.Cleanup(func() { _ = sqlBroker.Close() })
	for name, broker := range map[string]embedqueue.Broker{"mem": embedqueue.NewMemBroker(3), "sqlite": sqlBroker} {
		t.Run(name, func(t *testing.T) {
			perChunk5 := embedqueue.Job{CorpusID: "c", Source: "local", ChunkID: 5, IndexKind: "text", EmbedIdentity: lcIdentity}
			if err := broker.Enqueue(ctx, perChunk5); err != nil {
				t.Fatalf("enqueue per-chunk 5: %v", err)
			}
			if err := broker.Enqueue(ctx, documentJob(7, 5, 6, 7)); err != nil {
				t.Fatalf("enqueue document job: %v", err)
			}
			st, err := broker.Stats(ctx)
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if st.Pending != 2 {
				t.Fatalf("a document job keyed by its representation must not be swallowed by a per-chunk job sharing its first chunk id: %+v", st)
			}
			// And the same document job again IS a duplicate (keyed by rep).
			if err := broker.Enqueue(ctx, documentJob(7, 6, 7)); err != nil {
				t.Fatalf("enqueue second document job: %v", err)
			}
			if st, _ = broker.Stats(ctx); st.Pending != 2 {
				t.Fatalf("a second document job for rep 7 must dedup: %+v", st)
			}
			if err := broker.Enqueue(ctx, documentJob(8, 9)); err != nil {
				t.Fatalf("enqueue document job rep 8: %v", err)
			}
			if st, _ = broker.Stats(ctx); st.Pending != 3 {
				t.Fatalf("rep 8 document job must enqueue: %+v", st)
			}
			// Reverse order: a live DOCUMENT job whose first chunk is 9 must swallow an
			// incoming per-chunk job for chunk 9 on both brokers, because the SQLite
			// probe compares chunk_id against every live row and a document job's
			// chunk_id column is its first chunk id. Rep 8 (first chunk 9) is live
			// from above.
			if err := broker.Enqueue(ctx, embedqueue.Job{CorpusID: "c", Source: "local", ChunkID: 9, IndexKind: "text", EmbedIdentity: lcIdentity}); err != nil {
				t.Fatalf("enqueue per-chunk 9: %v", err)
			}
			if st, _ = broker.Stats(ctx); st.Pending != 3 {
				t.Fatalf("a per-chunk job for the first chunk of a live document job must dedup on both brokers: %+v", st)
			}
		})
	}
}
