package tests

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #684: `dir2mcp_list_files` resolved EVERY row against the local
// filesystem. An object-store corpus has no local file at RootDir/rel_path
// (S3FS.Walk ignores RootDir and reports no local path, the same asymmetry that
// broke the on-demand media paths in #759), so the gate condemned every remote
// document. The user saw an empty inventory and a `total` that counted rows the
// listing then refused to show, on a corpus that indexed and searched fine.
//
// The fix gates that filesystem check on the live `source.kind`. These tests
// pin both halves: remote objects become listable, and the path-shape and
// archive-member rules that protect the listing stay in force.

// listFilesOn684 calls dir2mcp_list_files against a server built from cfg and
// st, and returns the structured page.
func listFilesOn684(t *testing.T, cfg config.Config, st model.Store, args string) listFilesPageResult694 {
	t.Helper()
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	session := initializeSession(t, server.URL+cfg.MCPPath)
	body := `{"jsonrpc":"2.0","id":684,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{` + args + `}}}`
	resp := postRPC(t, server.URL+cfg.MCPPath, session, body)
	defer func() { _ = resp.Body.Close() }()

	return decodeListFilesEnvelope694(t, resp.Body)
}

// seedSourceTypedDoc684 records an indexed row with an explicit source_type. It
// is the state an S3 corpus is in after a successful scan: the store normalizes
// source_type to filesystem or archive_member, so a remote object is persisted
// as "filesystem" and carries no per-backend marker.
func seedSourceTypedDoc684(t *testing.T, st model.Store, relPath, sourceType string) {
	t.Helper()
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: relPath, DocType: "md", SourceType: sourceType,
		SizeBytes: 12, MTimeUnix: 1700000000, ContentHash: "seed-684", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert document %q: %v", relPath, err)
	}
}

// TestListFilesS3CorpusListsRemoteObjects_684 is the reproduction. RootDir is a
// real but EMPTY directory, so the root resolves and the only reason a row can
// disappear is the missing local RootDir/rel_path file, which is exactly the
// bug. Before the fix this listing came back with zero files.
func TestListFilesS3CorpusListsRemoteObjects_684(t *testing.T) {
	objects := map[string][]byte{
		"docs/a.md":          []byte("alpha"),
		"docs/nested/b.md":   []byte("bravo"),
		"reports/2026/c.txt": []byte("charlie"),
	}
	c := newS3Corpus(t, objects)
	for rel := range objects {
		seedDoc(t, c.store, rel, "md", 8)
	}

	page := listFilesOn684(t, c.cfg, c.store, `"limit":50,"offset":0`)

	assertRelPathSet694(t, "s3 corpus listing", page, []string{
		"docs/a.md", "docs/nested/b.md", "reports/2026/c.txt",
	})
	if page.Total != 3 {
		t.Fatalf("total=%d want 3", page.Total)
	}
}

// malformedStore684 injects rows the store itself refuses to persist.
// UpsertDocument validates rel_path, so a traversal or absolute path can only
// reach the tool through a store that hands one back. That is the shape of the
// stale or hand-edited row the path-shape check exists for.
type malformedStore684 struct {
	*store.SQLiteStore
	extra []model.Document
}

func (m *malformedStore684) ListVisibleFiles(ctx context.Context, prefix, glob string, limit, offset int, includeHidden bool) ([]model.Document, int64, error) {
	docs, total, err := m.SQLiteStore.ListVisibleFiles(ctx, prefix, glob, limit, offset, includeHidden)
	if err != nil {
		return nil, 0, err
	}
	return append(docs, m.extra...), total + int64(len(m.extra)), nil
}

// TestListFilesS3CorpusKeepsPathProtections_684 pins what the remote gate must
// NOT relax. A traversal or absolute rel_path can never round-trip through
// open_file on any backend, so it stays excluded. A hidden path stays hidden
// while include_hidden is false. An archive member stays listable, as it is on
// a local corpus.
func TestListFilesS3CorpusKeepsPathProtections_684(t *testing.T) {
	c := newS3Corpus(t, map[string][]byte{"docs/a.md": []byte("alpha")})
	seedSourceTypedDoc684(t, c.store, "docs/a.md", "filesystem")
	seedSourceTypedDoc684(t, c.store, "bundle.zip/inner/note.md", "archive_member")
	seedSourceTypedDoc684(t, c.store, "docs/.secret/key.txt", "filesystem")

	st := &malformedStore684{SQLiteStore: c.store, extra: []model.Document{
		{RelPath: "../escape.md", DocType: "md", SourceType: "filesystem", Status: "ok"},
		{RelPath: "/etc/passwd", DocType: "md", SourceType: "filesystem", Status: "ok"},
	}}

	page := listFilesOn684(t, c.cfg, st, `"limit":50,"offset":0`)

	assertRelPathSet694(t, "s3 corpus protections", page, []string{
		"docs/a.md", "bundle.zip/inner/note.md",
	})
}

// TestListFilesLocalCorpusStillDropsMissingFiles_684 is the control: the #176
// round-trip gate must keep running for a local corpus. The fixture differs
// from the one above by source.kind alone, so a fix that simply deleted the
// gate would fail here.
func TestListFilesLocalCorpusStillDropsMissingFiles_684(t *testing.T) {
	c := newS3Corpus(t, map[string][]byte{"docs/a.md": []byte("alpha")})
	seedDoc(t, c.store, "docs/a.md", "md", 5)

	cfg := c.cfg
	cfg.Source.Kind = "local"

	page := listFilesOn684(t, cfg, c.store, `"limit":50,"offset":0`)

	if len(page.Files) != 0 {
		t.Fatalf("a local corpus must still drop a row with no file under root, got %+v", page.Files)
	}
}
