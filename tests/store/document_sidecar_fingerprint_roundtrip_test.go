package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_DocumentSidecarFingerprintRoundtrip verifies that the
// sidecar_fingerprint column (added for #298) persists across upsert and is
// returned by both GetDocumentByPath and ListFiles. Unlike title, the
// fingerprint must be REPLACED unconditionally on every upsert — including being
// cleared back to empty when a sidecar is removed — so the remote ETag fast path
// never compares against a stale value.
func TestSQLiteStore_DocumentSidecarFingerprintRoundtrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	withSidecar := model.Document{
		RelPath:            "media/clip.mp4",
		DocType:            "video",
		SizeBytes:          4096,
		MTimeUnix:          1_700_000_000,
		SidecarFingerprint: "media/clip.en.vtt@1700000000\nmedia/clip.ru.srt@1700000050",
		Status:             "ok",
	}
	noSidecar := model.Document{
		RelPath:   "media/lonely.mp3",
		DocType:   "audio",
		SizeBytes: 2048,
		MTimeUnix: 1_700_000_001,
		Status:    "ok",
	}
	for _, d := range []model.Document{withSidecar, noSidecar} {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.RelPath, err)
		}
	}

	assertGetFingerprint(t, ctx, st, withSidecar.RelPath, withSidecar.SidecarFingerprint)
	assertGetFingerprint(t, ctx, st, noSidecar.RelPath, "")
	assertListFilesFingerprint(t, ctx, st, withSidecar)
	assertUpsertClearsFingerprint(t, ctx, st, withSidecar)
}

func assertGetFingerprint(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath, want string) {
	t.Helper()
	got, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if got.SidecarFingerprint != want {
		t.Errorf("doc %s: sidecar_fingerprint mismatch\n got: %q\nwant: %q", relPath, got.SidecarFingerprint, want)
	}
}

func assertListFilesFingerprint(t *testing.T, ctx context.Context, st *store.SQLiteStore, withSidecar model.Document) {
	t.Helper()
	docs, _, err := st.ListFiles(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.RelPath != withSidecar.RelPath {
			continue
		}
		found = true
		if d.SidecarFingerprint != withSidecar.SidecarFingerprint {
			t.Errorf("ListFiles: sidecar_fingerprint mismatch: got %q want %q", d.SidecarFingerprint, withSidecar.SidecarFingerprint)
		}
	}
	if !found {
		t.Fatalf("ListFiles did not return %q", withSidecar.RelPath)
	}
}

// assertUpsertClearsFingerprint upserts the same row with an empty fingerprint
// (mimicking a removed sidecar) and asserts the stored value is cleared — the
// always-replace semantics that distinguish it from title's CASE-preserve write.
func assertUpsertClearsFingerprint(t *testing.T, ctx context.Context, st *store.SQLiteStore, withSidecar model.Document) {
	t.Helper()
	cleared := withSidecar
	cleared.SidecarFingerprint = ""
	if err := st.UpsertDocument(ctx, cleared); err != nil {
		t.Fatalf("UpsertDocument(cleared): %v", err)
	}
	got, err := st.GetDocumentByPath(ctx, withSidecar.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath after clear: %v", err)
	}
	if got.SidecarFingerprint != "" {
		t.Errorf("sidecar_fingerprint not cleared on re-upsert: got %q want empty", got.SidecarFingerprint)
	}
}
