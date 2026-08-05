package embedqueue_test

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/identity"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// testCorpus is a REAL corpus: its own SQLite metadata store, its own chunk ids
// (SQLite rowids, so two corpora independently start at 1), and the corpus id
// production resolves from that store.
//
// The isolation bugs in #708/#709/#710 are all about a job carrying too little
// about which corpus and which payload it names, and none of them reproduce
// against a hand-built fake job: they need the real store's id allocation, its
// in-place (rep_id, ordinal) upsert, and its pending-selection. So the fixture
// builds the real thing.
type testCorpus struct {
	name  string
	store *store.SQLiteStore
	id    string
	rep   int64
}

// newTestCorpus creates a corpus rooted at a distinct path, with one
// representation ready to hold chunks.
func newTestCorpus(t *testing.T, name string) *testCorpus {
	t.Helper()
	ctx := context.Background()

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.db"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("%s: init store: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	relPath := "notes.md"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "local", Status: "ok", Title: name,
	}); err != nil {
		t.Fatalf("%s: seed document: %v", name, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("%s: read document: %v", name, err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID: doc.DocID, RepType: "raw_text", RepHash: "rep-" + name,
	})
	if err != nil {
		t.Fatalf("%s: seed representation: %v", name, err)
	}

	// The corpus id comes from the same call production uses, against this
	// corpus's own store, so the test exercises the real derive-and-persist path
	// rather than inventing an id.
	corpusID, err := identity.ResolveCorpusID(ctx, st, identity.CorpusKey("/corpora/"+name))
	if err != nil {
		t.Fatalf("%s: resolve corpus id: %v", name, err)
	}

	return &testCorpus{name: name, store: st, id: corpusID, rep: repID}
}

// addChunk writes a pending chunk at the given ordinal and returns its
// store-allocated chunk id. Re-calling it with the same ordinal is the in-place
// re-ingest #710 is about: the chunk id is preserved while text, hash, axis and
// status are all replaced.
func (c *testCorpus) addChunk(t *testing.T, ordinal int, text, hash, indexKind string) uint64 {
	t.Helper()
	id, err := c.store.InsertChunkWithSpans(context.Background(), model.Chunk{
		RepID: c.rep, Ordinal: ordinal, Text: text, TextHash: hash,
		IndexKind: indexKind, EmbeddingStatus: "pending",
	}, nil)
	if err != nil {
		t.Fatalf("%s: insert chunk: %v", c.name, err)
	}
	return uint64(id)
}

// pendingIDs returns the chunk ids the embed pipeline still considers pending.
func (c *testCorpus) pendingIDs(t *testing.T) []uint64 {
	t.Helper()
	tasks, err := c.store.NextPending(context.Background(), 100, "")
	if err != nil {
		t.Fatalf("%s: NextPending: %v", c.name, err)
	}
	ids := make([]uint64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.Metadata.ChunkID)
	}
	return ids
}

// isPending reports whether chunkID is still awaiting embedding.
func (c *testCorpus) isPending(t *testing.T, chunkID uint64) bool {
	t.Helper()
	for _, id := range c.pendingIDs(t) {
		if id == chunkID {
			return true
		}
	}
	return false
}

// failureCategories returns the per-category count of chunks recorded as
// terminally failed — the same aggregate `dir2mcp status` and `doctor` render.
func (c *testCorpus) failureCategories(t *testing.T) map[string]int64 {
	t.Helper()
	stats, err := c.store.CorpusStats(context.Background())
	if err != nil {
		t.Fatalf("%s: CorpusStats: %v", c.name, err)
	}
	if stats.FailureSummary == nil {
		return map[string]int64{}
	}
	return stats.FailureSummary.Categories
}

// recordingEmbedder is an Embedder bound to ONE corpus's store. It records the
// text of everything it embeds and marks those chunks embedded in that store, so
// a cross-corpus mix-up shows up both as recorded text from the wrong corpus and
// as a status write in the wrong store.
type recordingEmbedder struct {
	mu       sync.Mutex
	corpus   *testCorpus
	embedded []string
	failNext func(tasks []model.ChunkTask) error
}

func (e *recordingEmbedder) EmbedAndIndex(ctx context.Context, _ string, tasks []model.ChunkTask) (int, error) {
	e.mu.Lock()
	fail := e.failNext
	e.mu.Unlock()
	if fail != nil {
		if err := fail(tasks); err != nil {
			return 0, err
		}
	}

	labels := make([]uint64, 0, len(tasks))
	texts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		labels = append(labels, task.Metadata.ChunkID)
		texts = append(texts, task.Text)
	}
	// Mark BEFORE recording: a test that waits on the recording would otherwise
	// observe the embed and inspect chunk status before this write landed.
	if err := e.corpus.store.MarkEmbedded(ctx, labels); err != nil {
		return 0, err
	}
	e.mu.Lock()
	e.embedded = append(e.embedded, texts...)
	e.mu.Unlock()
	return len(tasks), nil
}

func (e *recordingEmbedder) texts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.embedded))
	copy(out, e.embedded)
	return out
}

// discardLogger keeps the rejection/dead-letter logging these tests deliberately
// provoke out of the test output.
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// runWorkerFor runs the worker loop for a fixed window and waits for it to exit.
// A fixed window (rather than "until it does the right thing") is what lets a
// test assert a NEGATIVE: the worker was given every chance to misbehave.
func runWorkerFor(t *testing.T, cfg embedqueue.Config, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = embedqueue.Run(ctx, cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d + 5*time.Second):
		t.Fatal("worker did not exit after its context was cancelled")
	}
}

// runWorkerUntilOrTimeout runs the worker loop until done() reports true (then
// stops it) or the timeout expires. Unlike runWorkerFor it stops as soon as the
// expected state is reached, so a positive assertion does not pay the full
// window.
func runWorkerUntilOrTimeout(t *testing.T, cfg embedqueue.Config, timeout time.Duration, done func() bool) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		_ = embedqueue.Run(ctx, cfg)
		close(finished)
	}()
	deadline := time.After(timeout)
	for {
		if done() {
			cancel()
			<-finished
			return true
		}
		select {
		case <-deadline:
			cancel()
			<-finished
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}
