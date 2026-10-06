package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §7.8 change-detection identity: for a local/NFS corpus the cheap
// pre-check is (size, mtime). dir2mcp applies it to MEDIA documents, whose read
// is the whole file; text documents keep confirming by content hash (#667).

func newStatFastPathHarness(t *testing.T) (string, *store.SQLiteStore, config.Config) {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	return root, st, cfg
}

func runWithFakeSTT(t *testing.T, cfg config.Config, st *store.SQLiteStore, text string) *fakeTranscriber {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	tr := &fakeTranscriber{text: text}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return tr
}

// TestMediaStatFastPath_UnchangedMediaIsNotReread pins the fast path: a media
// file whose size and mtime match the recorded document is neither re-read nor
// re-hashed on the next scan. The proof is a byte change that keeps both stat
// fields: without the fast path the hash would change and STT would run again.
func TestMediaStatFastPath_UnchangedMediaIsNotReread(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, st, cfg := newStatFastPathHarness(t)
	media := filepath.Join(root, "audio", "one.mp3")
	mustWriteFile(t, media, []byte("fake-audio-one"))
	stamp := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(media, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	runWithFakeSTT(t, cfg, st, "[00:00] first")
	doc1, _ := st.GetDocumentByPath(ctx, "audio/one.mp3")
	if doc1.ContentHash == "" || doc1.MTimeUnix != stamp.Unix() {
		t.Fatalf("first run must record hash and mtime, got %+v", doc1)
	}

	// Same length, different bytes, same mtime: the cheap signal says unchanged.
	if err := os.WriteFile(media, []byte("fake-audio-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(media, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	tr := runWithFakeSTT(t, cfg, st, "[00:00] second")
	doc2, _ := st.GetDocumentByPath(ctx, "audio/one.mp3")
	if tr.calls != 0 {
		t.Fatalf("unchanged (size, mtime) media must not be re-read or re-transcribed, got %d STT calls", tr.calls)
	}
	if doc2.ContentHash != doc1.ContentHash {
		t.Fatalf("fast path must leave the recorded identity untouched: %q -> %q", doc1.ContentHash, doc2.ContentHash)
	}

	// A new mtime is a change signal: the file is re-read and re-derived.
	later := stamp.Add(time.Hour)
	if err := os.Chtimes(media, later, later); err != nil {
		t.Fatal(err)
	}
	tr = runWithFakeSTT(t, cfg, st, "[00:00] third")
	doc3, _ := st.GetDocumentByPath(ctx, "audio/one.mp3")
	if tr.calls == 0 {
		t.Fatalf("a changed mtime must re-read the media and re-run STT")
	}
	if doc3.ContentHash == doc1.ContentHash {
		t.Fatalf("re-read must record the new bytes' hash")
	}
}

// TestMediaStatFastPath_TextStillConfirmsByHash is the control for #667: a text
// file edited in place with the same size and the same mtime is still re-hashed
// and its content_hash updated, because the stat fast path is media-only.
func TestMediaStatFastPath_TextStillConfirmsByHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, st, cfg := newStatFastPathHarness(t)
	text := filepath.Join(root, "notes", "a.txt")
	mustWriteFile(t, text, []byte("AAAAAAAAAA"))
	stamp := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(text, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := mustNewIngestService(t, cfg, st).Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	before, _ := st.GetDocumentByPath(ctx, "notes/a.txt")

	if err := os.WriteFile(text, []byte("BBBBBBBBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(text, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := mustNewIngestService(t, cfg, st).Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	after, _ := st.GetDocumentByPath(ctx, "notes/a.txt")
	if after.ContentHash == before.ContentHash || after.ContentHash == "" {
		t.Fatalf("a text file must still be confirmed by hash: %q -> %q", before.ContentHash, after.ContentHash)
	}
}
