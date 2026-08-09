package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #764: #727/#761 taught `reindex` to finish the rollback a crashed run
// owed, which covers the operator whose next move is another rebuild. The
// operator whose next move is restarting the daemon got the opposite outcome:
// `up` opened the live slot, which after a crash holds the crashed run's PARTIAL
// output, while the complete generation sat untouched beside it as
// *.reindex-old. Nothing errored; retrieval simply answered from a corpus that
// was missing documents.
//
// These tests seed a crash the same way the #727 suite does, by leaving the
// interrupted run's artifacts in place and never calling the rollback path
// (which is exactly what does not run on a crash), and then start the server.

// idleIngestor stands in for the ingestion pipeline so a test server never walks
// the corpus. Startup state is what is under test, and a real ingest pass would
// rewrite the very content hashes the assertions read.
type idleIngestor struct{}

func (idleIngestor) Run(context.Context) error     { return nil }
func (idleIngestor) Reindex(context.Context) error { return nil }

// recordingIndex is a model.Index + model.Persistable that records the bytes
// present at its snapshot path when the server rehydrates it. That is the
// question this issue is really about: not which files are on disk afterwards,
// but which generation the daemon actually opened to serve from.
//
// Save is a no-op so the persistence autosave cannot rewrite the live slot
// underneath the assertions.
type recordingIndex struct {
	path string

	mu     sync.Mutex
	loaded []byte
	onLoad func()
}

func (i *recordingIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }
func (i *recordingIndex) Delete(context.Context, []uint64) error                      { return nil }
func (i *recordingIndex) Search(context.Context, []float32, int, model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (i *recordingIndex) Identity(context.Context) (string, error) { return "", nil }
func (i *recordingIndex) Reset(context.Context, string) error      { return nil }
func (i *recordingIndex) Close() error                             { return nil }
func (i *recordingIndex) Save(context.Context, string) error       { return nil }

func (i *recordingIndex) Load(context.Context, string) error {
	data, err := os.ReadFile(i.path)
	i.mu.Lock()
	if err == nil {
		i.loaded = data
	}
	i.mu.Unlock()
	if i.onLoad != nil {
		i.onLoad()
	}
	return nil
}

func (i *recordingIndex) loadedString() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return string(i.loaded)
}

// upStartupProbe runs `dir2mcp up` in tmp far enough to open the corpus and then
// stops it, returning the exit code, stderr, and the per-kind indices it built.
// The server is cancelled as soon as an index has been rehydrated: everything
// this issue is about (recovery, store open, index load, content-hash rollback)
// happens before the serve loop, so idling in it would only make the suite slow.
func upStartupProbe(t *testing.T, tmp string) (int, string, map[string]*recordingIndex) {
	t.Helper()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	indices := map[string]*recordingIndex{}
	var indicesMu sync.Mutex
	loadedOnce := make(chan struct{})
	var closeLoaded sync.Once

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(config.Config, model.Store) (model.Ingestor, error) {
			return idleIngestor{}, nil
		},
		NewIndex: func(_ config.Config, kind string) (model.Index, string) {
			path := filepath.Join(stateDir, indexFileNameForKind(kind))
			ix := &recordingIndex{
				path:   path,
				onLoad: func() { closeLoaded.Do(func() { close(loadedOnce) }) },
			}
			indicesMu.Lock()
			indices[kind] = ix
			indicesMu.Unlock()
			return ix, path
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(15*time.Second))
		defer cancel()
		go func() {
			select {
			case <-loadedOnce:
			case <-ctx.Done():
			}
			cancel()
		}()
		code = app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"})
	})

	indicesMu.Lock()
	defer indicesMu.Unlock()
	return code, stderr.String(), indices
}

// seedReadyDocument stores one indexed document with a known content_hash and
// no crash artifacts beside it: the healthy corpus the recovery path must leave
// completely alone.
func seedReadyDocument(t *testing.T, stateDir, relPath string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("seed Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     relPath,
		DocType:     "md",
		ContentHash: crashGoodHash,
		Status:      "ready",
	}); err != nil {
		t.Fatalf("seed UpsertDocument: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
}

// indexFileNameForKind maps a vector index kind to the memory backend's snapshot
// basename, which is the name a crashed reindex leaves a backup beside.
func indexFileNameForKind(kind string) string {
	if kind == index.KindCode {
		return index.CodeIndexFileName
	}
	return index.TextIndexFileName
}

// TestUp_AfterCrashedReindex_ServesRecoveredGeneration is the #764 regression:
// a daemon started after a crashed reindex must open the last-known-good
// generation, not the partial one the crash left live, and must put the content
// hashes that describe it back too.
func TestUp_AfterCrashedReindex_ServesRecoveredGeneration(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	live := seedCrashedReindexState(t, stateDir, relPath)

	code, stderr, indices := upStartupProbe(t, tmp)
	if code != 0 {
		t.Fatalf("up should start after recovering a crashed reindex; got exit %d stderr=%q", code, stderr)
	}

	textIx := indices[index.KindText]
	if textIx == nil {
		t.Fatalf("no text index was built; stderr=%q", stderr)
	}
	if got := textIx.loadedString(); got != crashGoodIndex {
		t.Errorf("the daemon must serve the last-known-good generation, not the crashed run's partial output; want %q got %q", crashGoodIndex, got)
	}
	if got := readFileString(t, live); got != crashGoodIndex {
		t.Errorf("live index after startup must be the last-known-good generation; want %q got %q", crashGoodIndex, got)
	}
	if _, err := os.Stat(live + crashBackupSuffix); !os.IsNotExist(err) {
		t.Errorf("the backup slot must be empty once its generation is live again; stat err=%v", err)
	}
	// The file half without the store half would leave a complete index behind a
	// cleared gate, so the next ingest pass would re-process the whole corpus.
	if got := readDocumentHash(t, stateDir, relPath); got != crashGoodHash {
		t.Errorf("content_hash must be restored from the crashed run's snapshot; want %q got %q", crashGoodHash, got)
	}
	// Recovery is a mutation the operator did not ask for, so it has to announce
	// itself, by name, on the stream that is teed into server.log.
	if !strings.Contains(stderr, "interrupted reindex") || !strings.Contains(stderr, index.TextIndexFileName) {
		t.Errorf("startup must warn that it recovered an interrupted reindex and name what it touched; stderr=%q", stderr)
	}
}

// TestUp_HealthyCorpus_IsUntouchedByRecovery pins the other half of the
// contract: with no crash artifacts, startup neither writes anything nor says
// anything new.
func TestUp_HealthyCorpus_IsUntouchedByRecovery(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	live := filepath.Join(stateDir, index.TextIndexFileName)
	const healthy = "HEALTHY-INDEX"
	if err := os.WriteFile(live, []byte(healthy), 0o600); err != nil {
		t.Fatalf("seed live index: %v", err)
	}
	seedReadyDocument(t, stateDir, relPath)

	code, stderr, indices := upStartupProbe(t, tmp)
	if code != 0 {
		t.Fatalf("up should start on a healthy corpus; got exit %d stderr=%q", code, stderr)
	}
	if got := indices[index.KindText].loadedString(); got != healthy {
		t.Errorf("a healthy corpus must be opened as-is; want %q got %q", healthy, got)
	}
	if got := readFileString(t, live); got != healthy {
		t.Errorf("a healthy live index must not be rewritten; want %q got %q", healthy, got)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != crashGoodHash {
		t.Errorf("a healthy corpus's content_hash must not be touched; want %q got %q", crashGoodHash, got)
	}
	if strings.Contains(stderr, "interrupted reindex") {
		t.Errorf("a healthy corpus must not produce a recovery warning; stderr=%q", stderr)
	}
}

// TestUp_RecoveryFailure_RefusesToServe pins the fallback: when the
// last-known-good generation cannot be put back, the only thing left to serve is
// the partial one, so the server must refuse to start rather than come up
// quietly missing documents, and it must leave the good generation on disk for
// the operator to recover by hand.
func TestUp_RecoveryFailure_RefusesToServe(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	live := filepath.Join(stateDir, index.TextIndexFileName)
	// A non-empty directory in the live slot makes the restoring rename fail on
	// every platform, standing in for any reason the file cannot be put back
	// (permissions, a full disk, an ill-timed second process).
	if err := os.MkdirAll(filepath.Join(live, "occupied"), 0o700); err != nil {
		t.Fatalf("seed blocked live slot: %v", err)
	}
	if err := os.WriteFile(live+crashBackupSuffix, []byte(crashGoodIndex), 0o600); err != nil {
		t.Fatalf("seed backup index: %v", err)
	}

	code, stderr, indices := upStartupProbe(t, tmp)
	if code == 0 {
		t.Fatalf("up must not serve a corpus whose good generation could not be restored; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "recover interrupted reindex") {
		t.Errorf("the refusal must name its cause; stderr=%q", stderr)
	}
	if got := readFileString(t, live+crashBackupSuffix); got != crashGoodIndex {
		t.Errorf("a failed recovery must leave the last-known-good generation on disk; want %q got %q", crashGoodIndex, got)
	}
	if len(indices) != 0 {
		t.Errorf("the refusal must happen before any index is opened; built %d", len(indices))
	}
}
