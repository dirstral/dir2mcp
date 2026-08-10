package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// localEventSettleWindow is how long a test waits after it changes a local file
// before it asserts that the remote corpus did not change.
//
// The assertion is an absence, so the test cannot poll for it. The window must
// be long enough for the pre-#695 watcher to act: it debounces (10 ms here),
// then hands the job to its worker, which writes the store. One second is far
// more than that path needs.
const localEventSettleWindow = 1 * time.Second

// TestWatch_S3SourceIgnoresLocalFileEvents pins issue #695: with source.kind=s3,
// a local file event MUST NOT mutate the remote corpus.
//
// The setup is the reported failure shape. The corpus lives in an object store
// and holds docs/a.md. root_dir keeps its default meaning of "some local
// directory", and that directory happens to hold a file at the same relative
// path. Before the fix, Watch started fsnotify on that local directory, read the
// local delete as a corpus delete, and tombstoned the remote document. The
// remote object was still there, so retrieval hid a valid document until the next
// full rescan.
//
// The test asserts three things after a local delete and a local create:
//  1. the remote document is not tombstoned;
//  2. the local-only file is not ingested as a remote document;
//  3. the stats surface does not advertise a live watcher (SPEC §15.6).
func TestWatch_S3SourceIgnoresLocalFileEvents(t *testing.T) {
	// The remote corpus: one object, which stays present for the whole test.
	fs := newFakeRemoteFS()
	fs.add("docs/a.md", "etag-remote", "remote body")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "docs/a.md",
		DocType:     "text",
		SizeBytes:   int64(len("remote body")),
		ContentHash: ingest.ComputeContentHash([]byte("remote body")),
		ETag:        "etag-remote",
		Status:      "ok",
	})

	// The local directory the daemon runs from. It collides with the corpus on
	// docs/a.md, which is the whole hazard: the two paths name different things.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	localDoc := filepath.Join(root, "docs", "a.md")
	if err := os.WriteFile(localDoc, []byte("unrelated local body"), 0o600); err != nil {
		t.Fatalf("write local doc: %v", err)
	}

	cfg := config.Config{
		RootDir:             root,
		IngestWatch:         true,
		IngestWatchDebounce: 10 * time.Millisecond,
		Source: config.SourceConfig{
			Kind:     "s3",
			S3Bucket: "corpus-bucket",
		},
	}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetCorpusFS(fs)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)

	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan error, 1)
	go func() { watchDone <- svc.Watch(ctx) }()
	// Give a watcher time to register its directory watches, so the events below
	// are not missed for an uninteresting reason.
	time.Sleep(200 * time.Millisecond)

	// The two local events from the issue's reproduction: a delete of a path the
	// remote corpus also has, and a create of a path it does not.
	if err := os.Remove(localDoc); err != nil {
		t.Fatalf("remove local doc: %v", err)
	}
	localOnly := filepath.Join(root, "docs", "local-only.txt")
	if err := os.WriteFile(localOnly, []byte("local scratch file"), 0o600); err != nil {
		t.Fatalf("write local-only file: %v", err)
	}
	time.Sleep(localEventSettleWindow)

	cancel()
	if err := <-watchDone; err != nil {
		t.Fatalf("Watch returned error: %v", err)
	}

	doc, ok := st.get("docs/a.md")
	if !ok {
		t.Fatalf("remote document docs/a.md disappeared from the store")
	}
	if doc.Deleted {
		t.Errorf("a local delete tombstoned the remote document docs/a.md; the object is still in the bucket")
	}
	if _, ok := st.get("docs/local-only.txt"); ok {
		t.Errorf("a local file event ingested docs/local-only.txt into the remote-backed store; no such object exists")
	}
	if state.Snapshot().WatchActive {
		t.Errorf("watch reported active for an s3 corpus; no filesystem watcher governs that source (SPEC §15.6)")
	}
}

// TestSourceSupportsFileWatch pins which source kinds may drive the local
// filesystem watcher. local and nfs are ordinary directory trees under root_dir,
// so an fsnotify event names a real corpus path. s3 is not, so it must not.
// Matching is case-insensitive and an empty kind means local, mirroring
// corpusfs.New.
func TestSourceSupportsFileWatch(t *testing.T) {
	watchable := []string{"", "local", "nfs", "LOCAL", " NFS ", "Local"}
	for _, kind := range watchable {
		if !ingest.SourceSupportsFileWatch(kind) {
			t.Errorf("SourceSupportsFileWatch(%q) = false, want true: it is a filesystem corpus", kind)
		}
	}
	notWatchable := []string{"s3", "S3", " s3 ", "gcs"}
	for _, kind := range notWatchable {
		if ingest.SourceSupportsFileWatch(kind) {
			t.Errorf("SourceSupportsFileWatch(%q) = true, want false: it is not a local filesystem", kind)
		}
	}
}
