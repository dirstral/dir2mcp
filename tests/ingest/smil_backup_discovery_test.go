package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.1 (0.90.0): discovery skips the SMIL backups that the caption patch
// of §8.6.14 writes, `<smil>.bak.<YYYYmmdd_HHMMSS>[.<n>]`, whether or not
// write-back is enabled. The SMIL itself is still indexed.

func TestIsSMILBackup_MatchesOnlyTheBackupName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"v/abc.smil.bak.20251106_120000":    true,
		"v/abc.smil.bak.20251106_120000.1":  true,
		"v/abc.smil.bak.20251106_120000.12": true,
		"abc.smil.bak.20251106_120000":      true,
		"v/abc.smil":                        false,
		"v/abc.smil.bak":                    false,
		"v/abc.smil.bak.notes":              false,
		"v/abc.smil.bak.2025110_120000":     false,
		"v/abc.smil.bak.20251106_120000.x":  false,
		"v/abc.SMIL.BAK.20251106_120000":    false,
		"v/abc.ttml.bak.20251106_120000":    false,
		"v/abc.smil.bak.20251106_120000/x":  false,
	} {
		if got := corpusfs.IsSMILBackup(name); got != want {
			t.Errorf("IsSMILBackup(%q) = %v, want %v", name, got, want)
		}
	}
}

func newBackupTestStore(t *testing.T, stateDir string) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func runPlainIngest(t *testing.T, root, stateDir string, st *store.SQLiteStore) {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestDiscovery_SkipsSMILBackups indexes a corpus with write-back off. The two
// backups get no document row at all (not even a skipped one); the SMIL and a
// file that only looks like a backup are indexed.
func TestDiscovery_SkipsSMILBackups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil"), []byte(archiveSMIL))
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil.bak.20251106_120000"), []byte(archiveSMIL))
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil.bak.20251106_120000.1"), []byte(archiveSMIL))
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil.bak.notes"), []byte("not a backup name\n"))

	files, err := ingest.DiscoverFiles(ctx, root, 0)
	if err != nil {
		t.Fatal(err)
	}
	discovered := map[string]bool{}
	for _, f := range files {
		discovered[f.RelPath] = true
	}
	if discovered["v/abc.smil.bak.20251106_120000"] || discovered["v/abc.smil.bak.20251106_120000.1"] {
		t.Fatalf("discovery returned a SMIL backup: %v", files)
	}
	if !discovered["v/abc.smil"] || !discovered["v/abc.smil.bak.notes"] {
		t.Fatalf("discovery must keep the SMIL and a non-backup name: %v", files)
	}

	stateDir := testutil.TempDir(t)
	st := newBackupTestStore(t, stateDir)
	runPlainIngest(t, root, stateDir, st)
	for _, rel := range []string{"v/abc.smil.bak.20251106_120000", "v/abc.smil.bak.20251106_120000.1"} {
		if doc, err := st.GetDocumentByPath(ctx, rel); err == nil {
			t.Fatalf("%s must have no document row, got %+v", rel, doc)
		}
	}
	for _, rel := range []string{"v/abc.smil", "v/abc.smil.bak.notes"} {
		doc, err := st.GetDocumentByPath(ctx, rel)
		if err != nil || doc.Deleted || doc.Status != "ok" {
			t.Fatalf("%s must be indexed, got %+v (err=%v)", rel, doc, err)
		}
	}
}

// TestDiscovery_TombstonesABackupIndexedBefore pins the upgrade path: a backup
// that an earlier version indexed is no longer discovered, so the next scan
// tombstones it like any file that is gone.
func TestDiscovery_TombstonesABackupIndexedBefore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil"), []byte(archiveSMIL))
	mustWriteFile(t, filepath.Join(root, "v", "abc.smil.bak.20251106_120000"), []byte(archiveSMIL))

	stateDir := testutil.TempDir(t)
	st := newBackupTestStore(t, stateDir)
	const rel = "v/abc.smil.bak.20251106_120000"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: rel, DocType: "text", SourceType: "file", ContentHash: "stale", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	runPlainIngest(t, root, stateDir, st)
	doc, err := st.GetDocumentByPath(ctx, rel)
	if err != nil || !doc.Deleted {
		t.Fatalf("a backup indexed before must be tombstoned, got %+v (err=%v)", doc, err)
	}
}

// TestSMILPatch_BackupIsNeverIndexed runs the caption patch end to end: the
// patch writes a backup, and neither that scan nor the next one indexes it.
func TestSMILPatch_BackupIsNeverIndexed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := smilCorpus(t, archiveSMIL)
	stateDir := testutil.TempDir(t)
	st := runSMILCorpus(t, root, nil, stateDir)
	backups := smilBackups(t, root)
	if len(backups) != 1 {
		t.Fatalf("want one backup from the patch, got %v", backups)
	}
	runSMILCorpus(t, root, st, stateDir)
	rel, err := filepath.Rel(root, backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := st.GetDocumentByPath(ctx, filepath.ToSlash(rel)); err == nil {
		t.Fatalf("the patch backup %s must never be indexed, got %+v", rel, doc)
	}
	if doc, err := st.GetDocumentByPath(ctx, "v/abc.smil"); err != nil || doc.Deleted {
		t.Fatalf("the patched SMIL must stay indexed, got %+v (err=%v)", doc, err)
	}
}
