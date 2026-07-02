package ingest

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestExtractSingleCompressedMember_UnknownFormatErrors is the regression guard
// for the PR #483 finding: the format switch had no default case, so an
// unexpected format left the reader nil and io.LimitReader dereferenced nil,
// panicking. It must now return a clear error instead.
func TestExtractSingleCompressedMember_UnknownFormatErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(f, []byte("irrelevant"), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	members, err := extractSingleCompressedMember(f, "payload.bin", "xz")
	if err == nil {
		t.Fatal("want an error for an unsupported single-compressed format, got nil (nil-reader panic risk)")
	}
	if members != nil {
		t.Errorf("want no members on error, got %v", members)
	}
	if !strings.Contains(err.Error(), "unsupported single-compressed format") {
		t.Errorf("error = %q, want it to name the unsupported format", err.Error())
	}
}

// TestExtractSingleCompressedMember_EdgeCaseNameSanitised is the regression guard
// for the PR #483 path-traversal finding: a bare gzip named "..gz" strips to the
// traversal segment "." and would otherwise yield a "<archive>/." member
// rel_path. The member name must be replaced with a benign synthetic segment.
func TestExtractSingleCompressedMember_EdgeCaseNameSanitised(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte("edge case payload")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	f := filepath.Join(t.TempDir(), "..gz")
	if err := os.WriteFile(f, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write ..gz: %v", err)
	}

	members, err := extractSingleCompressedMember(f, "..gz", "gz")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	got := members[0].RelPath
	if got != "..gz/member" {
		t.Errorf("member rel_path = %q, want %q (sanitised synthetic name)", got, "..gz/member")
	}
	if strings.HasSuffix(got, "/..") || strings.HasSuffix(got, "/.") {
		t.Errorf("member rel_path %q ends in a traversal segment", got)
	}
}

func TestIsSafeArchiveMemberName(t *testing.T) {
	unsafe := []string{"", ".", "..", "../x", "a/b", `a\b`, "x..y"}
	for _, name := range unsafe {
		if isSafeArchiveMemberName(name) {
			t.Errorf("isSafeArchiveMemberName(%q) = true, want false", name)
		}
	}
	safe := []string{"member", "notes.txt", "data.out", ".hidden"}
	for _, name := range safe {
		if !isSafeArchiveMemberName(name) {
			t.Errorf("isSafeArchiveMemberName(%q) = false, want true", name)
		}
	}
}

// upsertSpyStore records UpsertDocument calls so a test can assert whether a
// durable status was persisted.
type upsertSpyStore struct {
	mu       sync.Mutex
	upserted []model.Document
}

func (s *upsertSpyStore) Init(context.Context) error { return nil }
func (s *upsertSpyStore) UpsertDocument(_ context.Context, doc model.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserted = append(s.upserted, doc)
	return nil
}
func (s *upsertSpyStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, nil
}
func (s *upsertSpyStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *upsertSpyStore) Close() error { return nil }

func (s *upsertSpyStore) statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.upserted))
	for _, d := range s.upserted {
		out = append(out, d.Status)
	}
	return out
}

func newArchiveTestService(t *testing.T, root string) (*Service, *upsertSpyStore) {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off" // keep construction credential-free
	spy := &upsertSpyStore{}
	svc, err := NewService(cfg, spy)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, spy
}

func writeZip(t *testing.T, path string, files map[string]string) {
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
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write zip: %v", err)
	}
}

// cancelAfterFirstCheck reports a live context on its first Err() call (so
// corpusFS.Localize proceeds) and context.Canceled on every subsequent call (so
// processArchiveMembers' per-member loop observes cancellation). It lets the test
// deterministically reach the mid-archive cancellation path the reviewer flagged,
// which a pre-cancelled context cannot (Localize swallows that as a non-fatal nil).
type cancelAfterFirstCheck struct {
	context.Context
	calls atomic.Int32
}

func (c *cancelAfterFirstCheck) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

// TestHandleArchiveDocument_CancellationNotPersisted is the regression guard for
// the PR #483 finding: processArchiveMembers returns ctx.Err() on cancellation,
// but handleArchiveDocument persisted status="error" for ANY error, so a
// shutdown mid-archive wrongly wrote a durable failure and mutated counters.
// Cancellation must now propagate without persisting state.
func TestHandleArchiveDocument_CancellationNotPersisted(t *testing.T) {
	root := t.TempDir()
	writeZip(t, filepath.Join(root, "docs.zip"), map[string]string{"notes.txt": "hi"})
	svc, spy := newArchiveTestService(t, root)

	ctx := &cancelAfterFirstCheck{Context: context.Background()}

	f := DiscoveredFile{RelPath: "docs.zip", AbsPath: filepath.Join(root, "docs.zip")}
	doc := model.Document{RelPath: "docs.zip", DocType: "archive", Status: "skipped"}

	err := svc.handleArchiveDocument(ctx, doc, f, nil, false, nil, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled to propagate, got %v", err)
	}
	for _, st := range spy.statuses() {
		if st == "error" {
			t.Errorf("cancellation wrongly persisted status=%q; no durable error must be written", st)
		}
	}
}

// TestHandleArchiveDocument_UnsupportedFormatPersistsError confirms the #398
// behavior is preserved: the unsupported-format sentinel still records the
// container as status="error".
func TestHandleArchiveDocument_UnsupportedFormatPersistsError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bundle.7z"), []byte("not a real 7z"), 0o600); err != nil {
		t.Fatalf("write 7z: %v", err)
	}
	svc, spy := newArchiveTestService(t, root)

	f := DiscoveredFile{RelPath: "bundle.7z", AbsPath: filepath.Join(root, "bundle.7z")}
	doc := model.Document{RelPath: "bundle.7z", DocType: "archive", Status: "skipped"}

	err := svc.handleArchiveDocument(context.Background(), doc, f, nil, false, nil, true)
	if err == nil {
		t.Fatal("want an error for the unsupported archive format, got nil")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unsupported format must not be reported as cancellation: %v", err)
	}
	found := false
	for _, st := range spy.statuses() {
		if st == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("unsupported archive must persist status=\"error\"; got upserts %v", spy.statuses())
	}
}
