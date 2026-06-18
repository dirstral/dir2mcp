package tests

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestEmbedWorkerEndToEnd_LiveInfra drives the real `dir2mcp embed-worker`
// subcommand against a live Tier-C pgvector store + real embed provider. It is
// the standalone worker role (SPEC §8.7.1) packaged without serving: it seeds a
// pending text chunk in the shared store, enqueues a job onto the sqlite broker
// (the path the subcommand reads by default), runs the subcommand for a bounded
// window, then asserts the chunk drained (embedding_status moved off pending).
//
// Gated behind RUN_INTEGRATION_TESTS=1 per the repo's integration-test policy,
// and requires DIR2MCP_INDEX_PGVECTOR_DSN (shared Tier-C store, §8.7.4) plus a
// resolvable embed credential (e.g. MISTRAL_API_KEY).
func TestEmbedWorkerEndToEnd_LiveInfra(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") == "" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to run integration tests")
	}
	dsn := os.Getenv("DIR2MCP_INDEX_PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("set DIR2MCP_INDEX_PGVECTOR_DSN to a live pgvector store to run the embed-worker e2e")
	}
	if os.Getenv("MISTRAL_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set MISTRAL_API_KEY (or OPENAI_API_KEY) so the worker can resolve an embed provider")
	}

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	// A corpus file backing the chunk (text chunks embed stored text directly,
	// but a real corpus keeps discovery/CorpusFS construction honest).
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello distributed embedding world"), 0o644); err != nil {
		t.Fatalf("write corpus file: %v", err)
	}

	// distributed_embed over a Tier-C backend with the dependency-free sqlite
	// broker — the one configuration the standalone worker requires. The pgvector
	// DSN is a runtime-only secret (§16.1.1) resolved from the environment
	// (DIR2MCP_INDEX_PGVECTOR_DSN, already set above), never the config file.
	cfgBody := "" +
		"root_dir: .\n" +
		"state_dir: .dir2mcp\n" +
		"index_backend: pgvector\n" +
		"distributed_embed_enabled: true\n" +
		"distributed_embed_broker: sqlite\n"
	writeWorkerConfig(t, tmp, cfgBody)

	chunkID := seedPendingTextChunk(t, stateDir)
	enqueueJobForChunk(t, stateDir, chunkID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var stdout, stderr capBuf
	app := cli.NewAppWithIO(&stdout, &stderr)

	done := make(chan int, 1)
	withWorkingDir(t, tmp, func() {
		go func() {
			done <- app.RunWithContext(ctx, []string{"embed-worker", "--poll-interval", "50ms"})
		}()

		// Poll the store until the chunk drains or the deadline fires.
		drained := waitForChunkDrain(t, stateDir, 45*time.Second)
		cancel()
		<-done
		if !drained {
			t.Fatalf("chunk %d did not drain within deadline; stderr=%s", chunkID, stderr.String())
		}
	})
}

// seedPendingTextChunk inserts one document/representation/chunk in pending
// embedding status and returns the chunk id.
func seedPendingTextChunk(t *testing.T, stateDir string) uint64 {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: "a.txt", DocType: "text", SourceType: "file",
		SizeBytes: 33, MTimeUnix: 1, ContentHash: "h1", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	repID, err := st.UpsertRepresentation(context.Background(), model.Representation{
		DocID: doc.DocID, RepType: "raw_text", RepHash: "rep-hash",
	})
	if err != nil {
		t.Fatalf("upsert representation: %v", err)
	}
	id, err := st.InsertChunkWithSpans(context.Background(), model.Chunk{
		RepID: repID, Ordinal: 0, Text: "hello distributed embedding world",
		TextHash: "chunk-hash", IndexKind: "text", EmbeddingStatus: "pending",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	return uint64(id)
}

// enqueueJobForChunk seeds the sqlite broker (at the subcommand's default path)
// with a job for chunkID, standing in for the coordinator the standalone worker
// deliberately does not run.
func enqueueJobForChunk(t *testing.T, stateDir string, chunkID uint64) {
	t.Helper()
	// Env-aware Load so the embed identity resolves from the provider credential
	// and the DSN secret, matching what the subcommand itself sees.
	cfg, err := config.Load(filepath.Join(filepath.Dir(stateDir), ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	broker, err := embedqueue.NewSQLiteBroker(context.Background(), filepath.Join(stateDir, "embed-queue.db"), cfg.DistributedEmbed.MaxAttempts)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	defer func() { _ = broker.Close() }()
	identity := cfg.Providers().EmbedIdentity()
	if err := broker.Enqueue(context.Background(), embedqueue.Job{
		ChunkID: chunkID, IndexKind: "text", EmbedIdentity: identity,
		CorpusID: cfg.RootDir, Source: cfg.Source.Kind, TextHash: "chunk-hash",
	}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
}

// waitForChunkDrain polls CorpusStats until at least one chunk is embedded or
// the deadline fires.
func waitForChunkDrain(t *testing.T, stateDir string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
		if err := st.Init(context.Background()); err != nil {
			t.Fatalf("init store for poll: %v", err)
		}
		stats, err := st.CorpusStats(context.Background())
		_ = st.Close()
		if err == nil && stats.EmbeddedOK >= 1 {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// capBuf is a tiny concurrency-safe buffer so the background subcommand
// goroutine and the assertions do not race on the writer.
type capBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
