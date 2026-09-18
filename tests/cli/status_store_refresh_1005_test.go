package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1005: `status` now re-reads the metadata store instead of trusting
// corpus.json, because the daemon stops refreshing that file when indexing
// reports stopped while the embed worker keeps draining for minutes. These
// tests pin what that change must NOT break: the run-liveness fields only the
// file carries, the fallback when the store cannot be read, and the support
// bundle, which ships the same payload. The counter parity itself is pinned in
// tests/mcp, because it takes both surfaces to see drift.

// runningCorpusJSON1005 is a cache that records indexing as running.
func runningCorpusJSON1005() string {
	return `{
  "ts": "2026-09-16T19:05:17Z",
  "indexing": {"mode":"full","running":true,"scanned":8,"indexed":8,"skipped":0,"deleted":0,
               "representations":0,"chunks_total":0,"embedded_ok":0,"embedded_pending":487,"errors":0,"unknown":0},
  "doc_counts": {"md": 1},
  "total_docs": 1,
  "code_ratio": 0.0
}`
}

// statusPayload1005 runs `status --json` against stateDir and returns the
// decoded envelope plus stderr.
func statusPayload1005(t *testing.T, stateDir string) (map[string]interface{}, string) {
	t.Helper()
	cfgPath := filepath.Join(filepath.Dir(stateDir), "dir2mcp.yaml")
	body := fmt.Sprintf("root_dir: %s\nstate_dir: %s\n", filepath.Dir(stateDir), stateDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	if code := app.RunWithContext(context.Background(),
		[]string{"--config", cfgPath, "--json", "status"}); code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode status json: %v raw=%s", err, stdout.String())
	}
	return payload, stderr.String()
}

func indexingOf1005(t *testing.T, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	snapshot, ok := payload["snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("no snapshot object: %#v", payload)
	}
	indexing, ok := snapshot["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("no indexing object: %#v", snapshot)
	}
	return indexing
}

// TestStatusKeepsRunLivenessFromCorpusJSON1005 guards #418 in reverse. mode and
// running describe the daemon's CURRENT run, and no store query can answer
// them, so refreshing the counters from the store must not drop them: a live
// indexing run has to keep reading as running.
func TestStatusKeepsRunLivenessFromCorpusJSON1005(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(runningCorpusJSON1005()), 0o644); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}
	// The test process is a guaranteed-live pid, standing in for the daemon
	// that owns this state dir, so the #418 stale-running reconciliation does
	// not fire and hide the regression this test looks for.
	pid := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(filepath.Join(stateDir, "server.pid"), []byte(pid), 0o644); err != nil {
		t.Fatalf("write server.pid: %v", err)
	}

	payload, _ := statusPayload1005(t, stateDir)
	if payload["source"] != "computed" {
		t.Fatalf("source=%v, want computed (the store was readable)", payload["source"])
	}
	indexing := indexingOf1005(t, payload)
	if indexing["running"] != true {
		t.Errorf("running=%v, want true (a live run must not read as stopped)", indexing["running"])
	}
	if indexing["mode"] != "full" {
		t.Errorf("mode=%v, want full (the run's mode, not the store's default)", indexing["mode"])
	}
	if payload["stale_running"] != nil {
		t.Errorf("stale_running=%v, want absent for a live daemon", payload["stale_running"])
	}
}

// TestStatusFallsBackToCorpusJSONWhenStoreUnreadable1005 pins the degraded
// path: an unreadable store costs freshness, not the command. status keeps the
// cached numbers, exits 0, and says on stderr that the numbers are cached, so
// the operator is never handed stale counters silently.
func TestStatusFallsBackToCorpusJSONWhenStoreUnreadable1005(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	// A directory named meta.sqlite passes the existence check and then fails
	// every read, which is the shape of a corrupt or half-written state dir.
	if err := os.MkdirAll(filepath.Join(stateDir, "meta.sqlite"), 0o755); err != nil {
		t.Fatalf("mkdir fake store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(runningCorpusJSON1005()), 0o644); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}

	payload, stderr := statusPayload1005(t, stateDir)
	if payload["source"] != "corpus_json" {
		t.Fatalf("source=%v, want corpus_json (the store could not be read)", payload["source"])
	}
	indexing := indexingOf1005(t, payload)
	if indexing["embedded_pending"] != float64(487) {
		t.Errorf("embedded_pending=%v, want the cached 487", indexing["embedded_pending"])
	}
	// The note has to SAY the numbers are cached. A bare store error would
	// leave the operator reading counters they have no reason to distrust,
	// which is the #1005 failure again in a smaller shape.
	if !strings.Contains(stderr, "reporting cached counters") {
		t.Errorf("stderr=%q, want a note that the reported counters are cached", stderr)
	}
}

// seedEmbeddedChunks1005 puts one document with `count` embedded chunks in the
// state dir, which is the corpus the store reports and the stale cache denies.
func seedEmbeddedChunks1005(t *testing.T, stateDir string, count int) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "docs/a.md", DocType: "md", SourceType: "filesystem", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	var ids []uint64
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "rep-a",
		})
		if rerr != nil {
			return rerr
		}
		for i := 0; i < count; i++ {
			id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
				RepID: repID, Ordinal: i,
				Text:     fmt.Sprintf("chunk %d", i),
				TextHash: fmt.Sprintf("th-%d", i),
				// MarkEmbedded is the only path to 'ok', so seed pending first.
				IndexKind: "text", EmbeddingStatus: "pending",
			}, nil)
			if cerr != nil {
				return cerr
			}
			ids = append(ids, uint64(id))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	if err := st.MarkEmbedded(ctx, ids); err != nil {
		t.Fatalf("mark embedded: %v", err)
	}
}

// TestSupportBundleStatusJSONReadsTheStore1005 pins the bundle to the same rule
// as the live command. A maintainer triaging a "stuck embedding" report reads
// status.json out of the bundle, so shipping the daemon's frozen cache there
// would reproduce #1005 inside the diagnostic meant to explain it.
func TestSupportBundleStatusJSONReadsTheStore1005(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	seedEmbeddedChunks1005(t, stateDir, 6)
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(runningCorpusJSON1005()), 0o600); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		if code := app.Run([]string{"support-bundle", "--output", bundlePath}); code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
	})

	var status struct {
		Source   string `json:"source"`
		Snapshot struct {
			Indexing struct {
				ChunksTotal     int64 `json:"chunks_total"`
				EmbeddedOK      int64 `json:"embedded_ok"`
				EmbeddedPending int64 `json:"embedded_pending"`
			} `json:"indexing"`
		} `json:"snapshot"`
	}
	entries := extractTarGz(t, bundlePath)
	if err := json.Unmarshal(entries["status.json"], &status); err != nil {
		t.Fatalf("decode status.json: %v body=%s", err, entries["status.json"])
	}
	if status.Source != "computed" {
		t.Errorf("source=%q, want computed", status.Source)
	}
	if status.Snapshot.Indexing.EmbeddedOK != 6 {
		t.Errorf("embedded_ok=%d, want 6 (the store, not the cached 0)", status.Snapshot.Indexing.EmbeddedOK)
	}
	if status.Snapshot.Indexing.ChunksTotal != 6 {
		t.Errorf("chunks_total=%d, want 6", status.Snapshot.Indexing.ChunksTotal)
	}
	if status.Snapshot.Indexing.EmbeddedPending != 0 {
		t.Errorf("embedded_pending=%d, want 0 (the cached 487 is long gone)", status.Snapshot.Indexing.EmbeddedPending)
	}
}
