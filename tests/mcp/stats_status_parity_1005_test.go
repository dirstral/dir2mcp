package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #1005: for ONE state dir at ONE moment, `dir2mcp status` and the
// dir2mcp_stats MCP tool reported different corpora.
//
//	$ dir2mcp status         ->  chunks=0  embedded=0  pending=487  errors=0
//	MCP dir2mcp_stats        ->  chunks_total=487  embedded_ok=487  errors=0
//
// The database agreed with the tool, and `ask` was answering with citations
// over those chunks. `status` printed the daemon's per-RUN counters (a run that
// embedded nothing, because `reindex` had run with the daemon down) next to a
// corpus-wide `pending` read from the store. "embedded=0 pending=487" reads as
// "indexing is stuck", which is the alarming direction of the same
// honest-reporting failure #676 and #413 fixed in the reassuring direction.
//
// These tests pin the two surfaces to the SAME numbers for the same state dir.
// Asserting `status` alone cannot catch drift, because drift is a statement
// about the pair.

// seedParityCorpus builds a state dir whose corpus is fully embedded: one
// document, one representation, embeddedOK chunks marked 'ok' and pending
// chunks left in the queue. It returns the state dir.
func seedParityCorpus(t *testing.T, embeddedOK, pending int) string {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "docs/a.md", DocType: "md", SourceType: "filesystem", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	var embedded []uint64
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "rep-a",
		})
		if rerr != nil {
			return rerr
		}
		for i := 0; i < embeddedOK+pending; i++ {
			id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
				RepID: repID, Ordinal: i,
				Text:     fmt.Sprintf("chunk %d", i),
				TextHash: fmt.Sprintf("th-%d", i),
				// A chunk only ever reaches 'ok' through MarkEmbedded, so it is
				// seeded pending and promoted below.
				IndexKind: "text", EmbeddingStatus: "pending",
			}, nil)
			if cerr != nil {
				return cerr
			}
			if i < embeddedOK {
				embedded = append(embedded, uint64(id))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	if len(embedded) > 0 {
		if err := st.MarkEmbedded(ctx, embedded); err != nil {
			t.Fatalf("mark embedded: %v", err)
		}
	}
	return stateDir
}

// writeStaleCorpusJSON plants the daemon-written cache in the exact #1005
// shape: the run that wrote it embedded nothing and chunked nothing, and the
// daemon stopped refreshing the file when indexing reported stopped, so it
// still says so long after the embed worker drained the queue.
func writeStaleCorpusJSON(t *testing.T, stateDir string, pending int) {
	t.Helper()
	raw := fmt.Sprintf(`{
  "ts": "2026-09-16T19:05:17Z",
  "indexing": {"mode":"incremental","running":false,"scanned":8,"indexed":8,"skipped":0,"deleted":0,
               "representations":0,"chunks_total":0,"embedded_ok":0,"embedded_pending":%d,"errors":0,"unknown":0},
  "doc_counts": {"md": 1},
  "total_docs": 1,
  "code_ratio": 0.0
}`, pending)
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}
}

// statusIndexingBlock runs `dir2mcp status --json` against stateDir and returns
// the indexing counter block.
func statusIndexingBlock(t *testing.T, stateDir string) map[string]interface{} {
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

	var payload struct {
		Snapshot struct {
			Indexing map[string]interface{} `json:"indexing"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode status json: %v raw=%s", err, stdout.String())
	}
	if payload.Snapshot.Indexing == nil {
		t.Fatalf("status emitted no indexing block: %s", stdout.String())
	}
	return payload.Snapshot.Indexing
}

// statsIndexingBlockOverStateDir calls dir2mcp_stats through the production
// chain (store aggregate -> retrieval.Stats -> tool) for the same state dir.
func statsIndexingBlockOverStateDir(t *testing.T, stateDir string) map[string]interface{} {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv, cfg := statsServerOverStore(t, stateDir, st)
	indexing, ok := callStatsTool(t, srv.URL+cfg.MCPPath)["indexing"].(map[string]interface{})
	if !ok {
		t.Fatal("dir2mcp_stats returned no indexing object")
	}
	return indexing
}

// TestStatusAndStatsAgreeOnOneStateDir1005 is the regression guard. It fails
// before the fix with status.embedded_ok=0 against stats.embedded_ok=487.
func TestStatusAndStatsAgreeOnOneStateDir1005(t *testing.T) {
	const embeddedOK = 487
	stateDir := seedParityCorpus(t, embeddedOK, 0)
	writeStaleCorpusJSON(t, stateDir, embeddedOK)

	status := statusIndexingBlock(t, stateDir)
	stats := statsIndexingBlockOverStateDir(t, stateDir)

	// Every counter, not just the one that broke: the defect was a whole block
	// taken from the wrong clock, so any single field could have been next.
	for _, field := range []string{
		"scanned", "indexed", "skipped", "deleted",
		"representations", "chunks_total", "embedded_ok", "errors",
	} {
		if status[field] != stats[field] {
			t.Errorf("drift on %s: status=%v dir2mcp_stats=%v (same state dir, same moment)",
				field, status[field], stats[field])
		}
	}

	// Pin the value too, not only the agreement: two surfaces that agree on a
	// wrong number are still wrong, and the store is the arbiter.
	if got := status["embedded_ok"]; got != float64(embeddedOK) {
		t.Errorf("status embedded_ok=%v, want %d (every seeded chunk is embedded)", got, embeddedOK)
	}
	if got := status["chunks_total"]; got != float64(embeddedOK) {
		t.Errorf("status chunks_total=%v, want %d", got, embeddedOK)
	}
}

// TestStatusPendingIsMeasuredWithEmbeddedOK1005 pins the second half of the
// report: `pending` and `embedded` must be read at the same instant from the
// same source. The stranded-queue state must still be reportable, so a corpus
// with real pending work has to show it.
func TestStatusPendingIsMeasuredWithEmbeddedOK1005(t *testing.T) {
	stateDir := seedParityCorpus(t, 3, 5)
	// A cache that claims the opposite of the store on BOTH counters.
	writeStaleCorpusJSON(t, stateDir, 487)

	status := statusIndexingBlock(t, stateDir)
	if got := status["embedded_ok"]; got != float64(3) {
		t.Errorf("embedded_ok=%v, want 3", got)
	}
	if got := status["embedded_pending"]; got != float64(5) {
		t.Errorf("embedded_pending=%v, want 5", got)
	}
	if got := status["chunks_total"]; got != float64(8) {
		t.Errorf("chunks_total=%v, want 8 (3 embedded + 5 pending)", got)
	}
}
