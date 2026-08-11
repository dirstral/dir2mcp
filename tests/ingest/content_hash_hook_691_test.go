package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #691: retrieval-time cross-file dedup (SPEC §9.2) grouped candidate hits
// with a rel_path → content_hash map that ingest never updated, so a live daemon
// grouped on the corpus as it was at startup. Ingest now reports the
// content_hash a document row holds after every durable write of that row.
//
// These tests pin the publication rule: a group key becomes visible only after
// the representations commit, and never while the document is in flight or
// failed.

// hashReport is one (rel_path, content_hash) pair the ingest hook reported.
type hashReport struct {
	relPath     string
	contentHash string
}

// contentHashReports records the reports of one ingest run, in order.
type contentHashReports struct {
	reports []hashReport
}

func (r *contentHashReports) record(relPath, contentHash string) {
	r.reports = append(r.reports, hashReport{relPath: relPath, contentHash: contentHash})
}

// hashesFor returns the reported hashes for one path, in order.
func (r *contentHashReports) hashesFor(relPath string) []string {
	out := make([]string, 0, len(r.reports))
	for _, report := range r.reports {
		if report.relPath == relPath {
			out = append(out, report.contentHash)
		}
	}
	return out
}

// writeDedupCorpusFile writes one file into root and returns the ingest input
// for it.
func writeDedupCorpusFile(t *testing.T, root, relPath, content string) ingest.DiscoveredFile {
	t.Helper()
	absPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
	return ingest.DiscoveredFile{AbsPath: absPath, RelPath: relPath, SizeBytes: int64(len(content))}
}

func assertStringSliceEqual(t *testing.T, got, want []string, stage string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: want %q, got %q", stage, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: want %q, got %q", stage, want, got)
		}
	}
}

// TestProcessDocument_ContentHashHook_PublishesOnlyAfterRepsCommit pins the
// publication point. Ingest withholds the content_hash done marker until the
// representations commit (#402), so the first report is empty and the real group
// key follows only after the commit.
func TestProcessDocument_ContentHashHook_PublishesOnlyAfterRepsCommit(t *testing.T) {
	root := t.TempDir()
	content := "alpha content for the dedup group"
	file := writeDedupCorpusFile(t, root, "a.txt", content)

	st := &fakeIncrementalStore{reflectByPath: true}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	reports := &contentHashReports{}
	svc.SetOnDocumentContentHash(reports.record)

	if err := svc.ProcessDocument(context.Background(), file, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	assertStringSliceEqual(t, reports.hashesFor("a.txt"),
		[]string{"", ingest.ComputeContentHash([]byte(content))},
		"a new document reports the withheld marker first")
}

// TestProcessDocument_ContentHashHook_NoPublishWhenRepsFail pins the fail-safe
// direction: a document whose representations did not commit keeps no group key,
// so retrieval passes it through instead of suppressing it.
func TestProcessDocument_ContentHashHook_NoPublishWhenRepsFail(t *testing.T) {
	root := t.TempDir()
	file := writeDedupCorpusFile(t, root, "a.txt", "alpha content for the dedup group")

	st := &fakeIncrementalStore{reflectByPath: true, insertChunkErr: errors.New("disk full")}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	reports := &contentHashReports{}
	svc.SetOnDocumentContentHash(reports.record)

	if err := svc.ProcessDocument(context.Background(), file, nil, false); err == nil {
		t.Fatal("a failed representation commit must return an error")
	}

	assertStringSliceEqual(t, reports.hashesFor("a.txt"), []string{""},
		"a failed document publishes no group key")
}

// TestProcessDocument_ContentHashHook_UnchangedDocumentRestatesItsHash pins the
// incremental path: a rescan that regenerates nothing still restates the hash
// the row holds, so a consumer that lost its state converges on the truth.
func TestProcessDocument_ContentHashHook_UnchangedDocumentRestatesItsHash(t *testing.T) {
	root := t.TempDir()
	content := "alpha content for the dedup group"
	file := writeDedupCorpusFile(t, root, "a.txt", content)
	hash := ingest.ComputeContentHash([]byte(content))

	st := &fakeIncrementalStore{existingDoc: model.Document{DocID: 10, RelPath: "a.txt", ContentHash: hash}}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	reports := &contentHashReports{}
	svc.SetOnDocumentContentHash(reports.record)

	if err := svc.ProcessDocument(context.Background(), file, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	assertStringSliceEqual(t, reports.hashesFor("a.txt"), []string{hash},
		"an unchanged document restates its hash once")
}

// TestLiveIngest_CrossFileDedupFollowsAnEditedDuplicate is the end-to-end case
// of #691, wired the way the server wires it: ingest reports every durable
// document write to retrieval.UpdateDocumentHash. Two byte-identical files
// collapse to one hit. One of them is edited, and both must be returned on the
// next query, with no restart of either service.
func TestLiveIngest_CrossFileDedupFollowsAnEditedDuplicate(t *testing.T) {
	root := t.TempDir()
	content := "alpha content for the dedup group"
	original := writeDedupCorpusFile(t, root, "a.txt", content)
	copyFile := writeDedupCorpusFile(t, root, "copy/a.txt", content)

	idx := &fixedLabelsIndex{labels: []uint64{1, 2}}
	ret := retrieval.NewService(nil, idx, &staticEmbedder{vec: []float32{1, 0}}, nil)
	ret.SetCrossFileDedupEnabled(true)
	ret.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.txt", Snippet: "alpha"})
	ret.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "copy/a.txt", Snippet: "alpha"})

	st := &fakeIncrementalStore{reflectByPath: true}
	svc := mustNewIngestService(t, config.Config{RootDir: root}, st)
	svc.SetOnDocumentContentHash(ret.UpdateDocumentHash)

	ctx := context.Background()
	if err := svc.ProcessDocument(ctx, original, nil, false); err != nil {
		t.Fatalf("ProcessDocument(a.txt): %v", err)
	}
	if err := svc.ProcessDocument(ctx, copyFile, nil, false); err != nil {
		t.Fatalf("ProcessDocument(copy/a.txt): %v", err)
	}

	assertLivePaths(t, ret, []string{"a.txt"}, "while both files are identical")

	edited := writeDedupCorpusFile(t, root, "copy/a.txt", "beta content, no longer a copy at all")
	if err := svc.ProcessDocument(ctx, edited, nil, false); err != nil {
		t.Fatalf("ProcessDocument(edited copy/a.txt): %v", err)
	}

	assertLivePaths(t, ret, []string{"a.txt", "copy/a.txt"}, "after the copy is edited")
}

// TestScan_OversizeFileForgetsItsGroupKey covers the discovery path. A file that
// grows past ingest.max_file_mb is rewritten as a skipped row with no
// content_hash, so it must give up its group key. Otherwise a live daemon keeps
// suppressing a distinct document against content the corpus no longer indexes.
func TestScan_OversizeFileForgetsItsGroupKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	oversize := bytes.Repeat([]byte("x"), 2*1024*1024) // 2 MiB, over the 1 MiB cap
	if err := os.WriteFile(filepath.Join(root, "big.txt"), oversize, 0o600); err != nil {
		t.Fatalf("write oversize file: %v", err)
	}

	// The corpus once held two byte-identical documents. ghost.txt is not on disk
	// any more, so this run never restates its key; big.txt is over the cap now.
	idx := &fixedLabelsIndex{labels: []uint64{1, 2}}
	ret := retrieval.NewService(nil, idx, &staticEmbedder{vec: []float32{1, 0}}, nil)
	ret.SetCrossFileDedupEnabled(true)
	ret.SetDocumentHashes([]model.DocumentHash{
		{RelPath: "ghost.txt", ContentHash: "H1"},
		{RelPath: "big.txt", ContentHash: "H1"},
	})
	ret.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "ghost.txt", Snippet: "alpha"})
	ret.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "big.txt", Snippet: "alpha"})

	assertLivePaths(t, ret, []string{"ghost.txt"}, "before the scan")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = 1
	svc := mustNewIngestService(t, cfg, st)
	svc.SetOnDocumentContentHash(ret.UpdateDocumentHash)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertLivePaths(t, ret, []string{"ghost.txt", "big.txt"}, "after the over-cap file is skipped")
}

// assertLivePaths runs one search and compares the surviving rel_paths.
func assertLivePaths(t *testing.T, ret *retrieval.Service, want []string, stage string) {
	t.Helper()
	hits, err := ret.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10})
	if err != nil {
		t.Fatalf("%s: Search: %v", stage, err)
	}
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.RelPath)
	}
	assertStringSliceEqual(t, got, want, stage)
}
