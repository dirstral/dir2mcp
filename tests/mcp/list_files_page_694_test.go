package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #694: every list_files call walked the ENTIRE matching corpus from store
// offset zero, in 500-row store pages, and ran filepath.EvalSymlinks on every
// row, purely to compute a post-filter total. `limit=1, offset=0` on a
// million-document corpus therefore cost ~2,000 store reads and ~1,000,000
// filesystem syscalls, and a client walking every page paid that per page,
// making the walk quadratic. The store-level pagination of #429 F10 was
// completely undone at the public tool boundary.
//
// The fix pushes the hidden-path predicate into SQL
// (store.SQLiteStore.ListVisibleFiles) so one tool call is one store call over
// one page, and the #176 round-trip gate runs only over the rows of that page.
//
// These tests are MCP-level on purpose: the store-level #429 tests already pass
// against the broken handler, because the defect was entirely in how the
// handler used the store.

// countingStore694 counts how the handler reaches the store. It embeds the real
// *store.SQLiteStore, so it is a fully functional model.Store; BOTH listing
// methods are overridden because embedding would otherwise satisfy
// ListVisibleFiles invisibly and the count would stay at zero.
type countingStore694 struct {
	*store.SQLiteStore
	listFilesCalls atomic.Int64
	visibleCalls   atomic.Int64
}

func (c *countingStore694) ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	c.listFilesCalls.Add(1)
	return c.SQLiteStore.ListFiles(ctx, prefix, glob, limit, offset)
}

func (c *countingStore694) ListVisibleFiles(ctx context.Context, prefix, glob string, limit, offset int, includeHidden bool) ([]model.Document, int64, error) {
	c.visibleCalls.Add(1)
	return c.SQLiteStore.ListVisibleFiles(ctx, prefix, glob, limit, offset, includeHidden)
}

// storeReads reports how many times the handler asked the store for rows,
// however it asked. The contract under test is "one page costs one store read",
// not "the handler uses a particular method".
func (c *countingStore694) storeReads() int64 {
	return c.listFilesCalls.Load() + c.visibleCalls.Load()
}

func TestListFilesServesASmallFirstPageInOneStoreRead_694(t *testing.T) {
	// 1200 documents is 3 pages of the old 500-row walk, so the pre-fix
	// handler cannot serve even `limit=5, offset=0` in fewer than 3 reads.
	counting, root, stateDir := seedCountingCorpus694(t, 1200)

	page := listFilesPage694(t, stateDir, root, counting, `"limit":5,"offset":0`)

	if got := counting.storeReads(); got != 1 {
		t.Fatalf("a 5-row first page cost %d store reads (ListFiles=%d ListVisibleFiles=%d); "+
			"the handler is still walking the whole corpus to compute a total (#694)",
			got, counting.listFilesCalls.Load(), counting.visibleCalls.Load())
	}
	if len(page.Files) != 5 {
		t.Fatalf("first page returned %d files, want 5", len(page.Files))
	}
	if page.Total != 1200 {
		t.Fatalf("total=%d want 1200", page.Total)
	}
	for i, f := range page.Files {
		if want := corpusRelPath694(i); f.RelPath != want {
			t.Fatalf("files[%d].rel_path=%q want %q", i, f.RelPath, want)
		}
	}
}

func TestListFilesDeepPageDoesNotRestartFromZero_694(t *testing.T) {
	counting, root, stateDir := seedCountingCorpus694(t, 1200)

	page := listFilesPage694(t, stateDir, root, counting, `"limit":5,"offset":1000`)

	if got := counting.storeReads(); got != 1 {
		t.Fatalf("offset=1000 cost %d store reads (ListFiles=%d ListVisibleFiles=%d); "+
			"a later page must be served by the store's own LIMIT/OFFSET, not by "+
			"re-walking every preceding row (#694)",
			got, counting.listFilesCalls.Load(), counting.visibleCalls.Load())
	}
	if len(page.Files) != 5 {
		t.Fatalf("deep page returned %d files, want 5", len(page.Files))
	}
	for i, f := range page.Files {
		if want := corpusRelPath694(1000 + i); f.RelPath != want {
			t.Fatalf("files[%d].rel_path=%q want %q", i, f.RelPath, want)
		}
	}
	if page.Total != 1200 {
		t.Fatalf("total=%d want 1200", page.Total)
	}
}

// TestListFilesHiddenFilteringSurvivesTheSQLPushdown_694 pins the translation
// itself: the SQL form must hide exactly the rows the Go form hides. Since #693
// the rule is "any segment that starts with a dot", so `visible/.config/b.md`
// is hidden too.
func TestListFilesHiddenFilteringSurvivesTheSQLPushdown_694(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, []model.Document{
		{RelPath: ".hidden/a.md", DocType: "md", MTimeUnix: 100, Status: "ok"},
		{RelPath: "visible/.config/b.md", DocType: "md", MTimeUnix: 200, Status: "ok"},
		{RelPath: "a.md", DocType: "md", MTimeUnix: 300, Status: "ok"},
		{RelPath: ".x", DocType: "txt", MTimeUnix: 400, Status: "ok"},
		{RelPath: "dir/sub/c.md", DocType: "md", MTimeUnix: 500, Status: "ok"},
	})

	excluded := listFilesPage694(t, tmp, root, st, `"limit":50,"offset":0,"include_hidden":false`)
	included := listFilesPage694(t, tmp, root, st, `"limit":50,"offset":0,"include_hidden":true`)

	// Exactly the set the Go predicate isListFilesNoisePath produces: no
	// segment that starts with a dot.
	wantVisible := []string{"a.md", "dir/sub/c.md"}
	wantAll := append([]string{".hidden/a.md", ".x", "visible/.config/b.md"}, wantVisible...)

	assertRelPathSet694(t, "include_hidden=false", excluded, wantVisible)
	assertRelPathSet694(t, "include_hidden=true", included, wantAll)

	if excluded.Total != int64(len(wantVisible)) {
		t.Fatalf("include_hidden=false total=%d want %d; the total must describe the "+
			"same set the page is drawn from", excluded.Total, len(wantVisible))
	}
	if included.Total != int64(len(wantAll)) {
		t.Fatalf("include_hidden=true total=%d want %d", included.Total, len(wantAll))
	}
}

// TestListFilesStillDropsRowsWhoseFileIsGone_694 is the #176 regression guard.
// Pushing the total down to SQL deliberately stops the total from accounting
// for tombstoning drift, but it must not weaken the guarantee that actually
// matters: every EMITTED path round-trips through open_file. That gate is
// page-scoped now, and page-scoped is enough.
func TestListFilesStillDropsRowsWhoseFileIsGone_694(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, []model.Document{
		{RelPath: "live/one.md", DocType: "md", MTimeUnix: 100, Status: "ok"},
		{RelPath: "live/two.md", DocType: "md", MTimeUnix: 200, Status: "ok"},
	})
	// A row whose backing file was never written: stale metadata of exactly the
	// kind #176 was filed about.
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: "live/ghost.md", DocType: "md", MTimeUnix: 300, Status: "ok",
	}); err != nil {
		t.Fatalf("seed ghost row: %v", err)
	}

	page := listFilesPage694(t, tmp, root, st, `"limit":50,"offset":0`)

	assertRelPathSet694(t, "listing", page, []string{"live/one.md", "live/two.md"})
}

// --- fixtures -------------------------------------------------------------

type listFilesPageResult694 struct {
	Limit  int64
	Offset int64
	Total  int64
	Files  []struct {
		RelPath string `json:"rel_path"`
		Status  string `json:"status"`
	}
}

func corpusRelPath694(i int) string {
	return fmt.Sprintf("docs/f%04d.md", i)
}

// seedCountingCorpus694 writes n real files plus their store rows and wraps the
// store in the counting decorator. Real files are mandatory: list_files
// resolves every row against the corpus root and drops what is not there, so a
// store-only fixture returns zero files and every assertion below would pass
// vacuously against broken code.
func seedCountingCorpus694(t *testing.T, n int) (*countingStore694, string, string) {
	t.Helper()
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(tmp, "corpus")
	docs := make([]model.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, model.Document{
			RelPath:   corpusRelPath694(i),
			DocType:   "md",
			MTimeUnix: int64(1700000000 + i),
			Status:    "ok",
		})
	}
	seedCorpus(t, st, root, docs)

	counting := &countingStore694{SQLiteStore: st}
	// Seeding wrote through the bare store; only handler traffic is counted.
	counting.listFilesCalls.Store(0)
	counting.visibleCalls.Store(0)
	return counting, root, tmp
}

// listFilesPage694 calls the real dir2mcp_list_files tool over HTTP. Going
// through the tool is the point: the defect lives in the handler's use of the
// store, so anything short of the tool boundary would miss it.
func listFilesPage694(t *testing.T, stateDir, root string, st model.Store, args string) listFilesPageResult694 {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{` + args + `}}}`
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("list_files status=%d body=%s", resp.StatusCode, payload)
	}

	return decodeListFilesEnvelope694(t, resp.Body)
}

func decodeListFilesEnvelope694(t *testing.T, body io.Reader) listFilesPageResult694 {
	t.Helper()
	var envelope struct {
		Result struct {
			StructuredContent listFilesPageResult694 `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		t.Fatalf("decode list_files response: %v", err)
	}
	return envelope.Result.StructuredContent
}

func assertRelPathSet694(t *testing.T, label string, page listFilesPageResult694, want []string) {
	t.Helper()
	got := make(map[string]bool, len(page.Files))
	for _, f := range page.Files {
		got[f.RelPath] = true
	}
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
		if !got[w] {
			t.Fatalf("%s: %q missing from the listing (got %v)", label, w, keysOf694(got))
		}
	}
	for g := range got {
		if !wanted[g] {
			t.Fatalf("%s: %q should not be listed (got %v, want %v)", label, g, keysOf694(got), want)
		}
	}
}

func keysOf694(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
