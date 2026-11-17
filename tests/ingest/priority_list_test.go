package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.1 Processing order: ingest.priority_list.

// runOrder runs one ingest of root with the given priority list setting and
// returns the rel_paths in the order the run manifest recorded them, which is
// the order the pass processed them.
func runOrder(t *testing.T, root, priorityList string) []string {
	t.Helper()
	ctx := context.Background()
	stateDir := filepath.Join(testutil.TempDir(t), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	manifestPath := filepath.Join(stateDir, "run.jsonl")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.MediaBatchManifest = manifestPath
	cfg.IngestPriorityList = priorityList

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var order []string
	for _, r := range readManifest(t, manifestPath) {
		rel, _ := r["rel_path"].(string)
		order = append(order, rel)
	}
	return order
}

func priorityCorpus(t *testing.T) string {
	t.Helper()
	root := testutil.TempDir(t)
	for _, rel := range []string{"a.txt", "b/c.txt", "b/d.txt", "e/f.txt", "g.txt"} {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(rel)), []byte("text of "+rel))
	}
	return root
}

// TestPriorityList_ListedFilesComeFirstInListOrder pins the order: files an
// entry covers come first, by the line of the first entry that covers them
// (a file entry or a directory entry), then the rest in relative-path order.
// Comments, blank lines, bad entries and entries that match nothing are
// ignored, and no file is added or dropped.
func TestPriorityList_ListedFilesComeFirstInListOrder(t *testing.T) {
	root := priorityCorpus(t)
	list := strings.Join([]string{
		"# newest first",
		"",
		"e/",
		"b/d.txt",
		"../outside",
		"/abs/path",
		"missing/dir",
		"b",
		"e/f.txt", // repeated: e/ already ranks it first
	}, "\n")
	mustWriteFile(t, filepath.Join(root, "order.txt"), []byte(list))

	got := strings.Join(runOrder(t, root, "order.txt"), ",")
	want := "e/f.txt,b/d.txt,b/c.txt,a.txt,g.txt,order.txt"
	if got != want {
		t.Fatalf("processing order = %s\nwant %s", got, want)
	}
}

// TestPriorityList_DefaultAndUnreadableKeepPathOrder pins that no list, and a
// list that cannot be read, keep relative-path order and do not fail the run.
func TestPriorityList_DefaultAndUnreadableKeepPathOrder(t *testing.T) {
	root := priorityCorpus(t)
	want := "a.txt,b/c.txt,b/d.txt,e/f.txt,g.txt"
	for name, list := range map[string]string{
		"unset":   "",
		"missing": filepath.Join(testutil.TempDir(t), "no-such-list.txt"),
	} {
		if got := strings.Join(runOrder(t, root, list), ","); got != want {
			t.Fatalf("%s: processing order = %s, want %s", name, got, want)
		}
	}
}

// TestPriorityList_ConfigKeyLoads pins the nested key and its default.
func TestPriorityList_ConfigKeyLoads(t *testing.T) {
	if d := config.Default(); d.IngestPriorityList != "" {
		t.Fatalf("default = %q, want empty", d.IngestPriorityList)
	}
	path := filepath.Join(testutil.TempDir(t), "cfg.yaml")
	if err := os.WriteFile(path, []byte("ingest:\n  priority_list: lists/newest.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil || loaded.IngestPriorityList != "lists/newest.txt" {
		t.Fatalf("nested key must load, got %q err=%v", loaded.IngestPriorityList, err)
	}
}
