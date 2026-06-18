package tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// readManifest parses a JSONL run manifest (SPEC §8.6.11) into one map per line.
func readManifest(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest %s: %v", path, err)
	}
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("manifest line not valid JSON (%q): %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// TestServiceRun_BatchManifest pins the JSONL run manifest (SPEC §8.6.11): a
// record per asset with the required fields, "skipped" status on a cache-hit
// second run, and truncate-and-rewrite each run.
func TestServiceRun_BatchManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha text"))
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), []byte("beta text"))

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	manifestPath := filepath.Join(stateDir, "run.jsonl")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.MediaBatchManifest = manifestPath
	cfg.MediaBatchProgress = true

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	svc := mustNewIngestService(t, cfg, st)

	// First run: both assets are newly processed -> completed.
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	recs := readManifest(t, manifestPath)
	if len(recs) != 2 {
		t.Fatalf("want 2 manifest records, got %d: %+v", len(recs), recs)
	}
	byPath := map[string]map[string]any{}
	for _, r := range recs {
		rel, _ := r["rel_path"].(string)
		byPath[rel] = r
	}
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		r, ok := byPath[p]
		if !ok {
			t.Fatalf("manifest missing record for %s; got %+v", p, byPath)
		}
		if r["status"] != "completed" {
			t.Fatalf("%s: status=%v, want completed", p, r["status"])
		}
		if h, _ := r["content_hash"].(string); h == "" {
			t.Fatalf("%s: missing content_hash (a §8.6.11 MUST field)", p)
		}
		if _, ok := r["processing_ms"]; !ok {
			t.Fatalf("%s: missing processing_ms", p)
		}
	}

	// Second run over the unchanged corpus: cache hits -> skipped, and the
	// manifest is truncated and rewritten (still exactly 2 records).
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	recs2 := readManifest(t, manifestPath)
	if len(recs2) != 2 {
		t.Fatalf("second run: want 2 records (truncated+rewritten), got %d", len(recs2))
	}
	for _, r := range recs2 {
		if r["status"] != "skipped" {
			t.Fatalf("second run %v: status=%v, want skipped (cache hit)", r["rel_path"], r["status"])
		}
	}
}

// TestServiceRun_BatchManifestDisabledWritesNothing pins that with the manifest
// unconfigured (default), no manifest file is written.
func TestServiceRun_BatchManifestDisabledWritesNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha text"))

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	candidate := filepath.Join(stateDir, "run.jsonl")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	// MediaBatchManifest left empty -> disabled.

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("manifest should not exist when disabled (stat err=%v)", err)
	}
}
