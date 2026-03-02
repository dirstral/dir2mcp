package tests

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"dir2mcp/internal/config"
	"dir2mcp/internal/model"
	"dir2mcp/internal/store"
)

// buildZip returns the bytes of a zip archive containing the provided files.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// buildTarGz returns the bytes of a .tar.gz archive containing the provided files.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		data := []byte(content)
		hdr := &tar.Header{Name: name, Size: int64(len(data)), Typeflag: tar.TypeReg, Mode: 0o644}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// runArchiveIngest creates a fresh store + service, writes archiveName with
// archiveData into a temp root dir, runs ingestion, and returns the store
// for assertions.
func runArchiveIngest(t *testing.T, archiveName string, archiveData []byte) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, archiveName), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

// docPaths returns a set of rel_path strings from the store.
func docPaths(t *testing.T, st *store.SQLiteStore) map[string]bool {
	t.Helper()
	docs, _, err := st.ListFiles(context.Background(), "", "", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	m := make(map[string]bool, len(docs))
	for _, d := range docs {
		m[d.RelPath] = true
	}
	return m
}

func documentByPath(t *testing.T, st *store.SQLiteStore, relPath string) model.Document {
	t.Helper()
	docs, _, err := st.ListFiles(context.Background(), "", "", 1000, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, doc := range docs {
		if doc.RelPath == relPath {
			return doc
		}
	}
	t.Fatalf("document not found: %s", relPath)
	return model.Document{}
}

// TestArchiveIngest_ZipMembersIndexed verifies that zip members are ingested
// as separate documents.
func TestArchiveIngest_ZipMembersIndexed(t *testing.T) {
	data := buildZip(t, map[string]string{
		"notes.txt": "hello from zip",
		"code.go":   "package main\nfunc main() {}",
	})
	st := runArchiveIngest(t, "docs.zip", data)
	paths := docPaths(t, st)

	if !paths["docs.zip/notes.txt"] {
		t.Errorf("want docs.zip/notes.txt indexed; got paths: %v", paths)
	}
	if !paths["docs.zip/code.go"] {
		t.Errorf("want docs.zip/code.go indexed; got paths: %v", paths)
	}
}

// TestArchiveIngest_TarGzMembersIndexed verifies tar.gz member ingestion.
func TestArchiveIngest_TarGzMembersIndexed(t *testing.T) {
	data := buildTarGz(t, map[string]string{
		"README.md": "# hello",
	})
	st := runArchiveIngest(t, "bundle.tar.gz", data)
	paths := docPaths(t, st)

	if !paths["bundle.tar.gz/README.md"] {
		t.Errorf("want bundle.tar.gz/README.md indexed; got paths: %v", paths)
	}
}

// TestArchiveIngest_ZipSlipRejected verifies that members with path traversal
// sequences are silently skipped.
func TestArchiveIngest_ZipSlipRejected(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// traversal member
	fw, _ := w.Create("../../etc/passwd")
	_, _ = fw.Write([]byte("malicious"))
	// safe member — should still be ingested
	fw2, _ := w.Create("safe.txt")
	_, _ = fw2.Write([]byte("safe content"))
	_ = w.Close()

	st := runArchiveIngest(t, "test.zip", buf.Bytes())
	paths := docPaths(t, st)

	for p := range paths {
		if p == "test.zip/../../etc/passwd" || p == "../../etc/passwd" {
			t.Errorf("zip-slip member must not be ingested, got %q", p)
		}
	}
	if !paths["test.zip/safe.txt"] {
		t.Errorf("want test.zip/safe.txt; zip-slip rejection must not drop safe members; got %v", paths)
	}
}

// TestArchiveIngest_MembersNotTombstonedOnRescan verifies that archive members
// survive a second scan without being tombstoned. This is a regression test
// for a bug where virtual paths (e.g. "docs.zip/notes.txt") were absent from
// the seen map and got deleted by markMissingAsDeleted.
func TestArchiveIngest_MembersNotTombstonedOnRescan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	archiveData := buildZip(t, map[string]string{
		"notes.txt": "hello from zip",
		"code.go":   "package main\nfunc main() {}",
	})
	if err := os.WriteFile(filepath.Join(root, "docs.zip"), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	// Use a single store across both scans so tombstoning is observable.
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)

	// First scan — members should be ingested.
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	paths1 := docPaths(t, st)
	if !paths1["docs.zip/notes.txt"] || !paths1["docs.zip/code.go"] {
		t.Fatalf("first scan: expected archive members; got %v", paths1)
	}

	// Second scan — same archive, no changes. Members must survive.
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	paths2 := docPaths(t, st)
	if !paths2["docs.zip/notes.txt"] {
		t.Errorf("docs.zip/notes.txt tombstoned after re-scan; got paths: %v", paths2)
	}
	if !paths2["docs.zip/code.go"] {
		t.Errorf("docs.zip/code.go tombstoned after re-scan; got paths: %v", paths2)
	}
}

// TestArchiveIngest_NestedArchiveNotRecursed verifies that archive members
// which are themselves archives are not recursively extracted (depth limit 1).
func TestArchiveIngest_NestedArchiveNotRecursed(t *testing.T) {
	innerZip := buildZip(t, map[string]string{"inner.txt": "deep"})
	outerZip := buildZip(t, map[string]string{"inner.zip": string(innerZip)})

	st := runArchiveIngest(t, "outer.zip", outerZip)
	paths := docPaths(t, st)

	if !paths["outer.zip/inner.zip"] {
		t.Error("nested archive member should be persisted as a depth-1 document")
	}
	if paths["outer.zip/inner.zip/inner.txt"] {
		t.Error("nested archive member must not be recursed into")
	}
	nested := documentByPath(t, st, "outer.zip/inner.zip")
	if nested.SourceType != "archive_member" {
		t.Fatalf("unexpected source_type for nested archive member: got=%q want=%q", nested.SourceType, "archive_member")
	}
}
