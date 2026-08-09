package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #781, counting half. PR #792 made a not-followed symlink visible in the log.
// It could not count the drop, because SPEC 15.2 kept the skip_reasons enum
// closed and no reason described a link the walker does not follow. Spec
// 0.46.0 added symlink_ignored, so the drop is now counted, persisted and
// reported as a file_skip event.
//
// TestSkipReasonCoverage asserts one file link inside a mixed corpus. These
// cover the contracts that mix cannot express: a corpus that is ONLY links
// (the case from the issue), a link to a directory, and follow_symlinks=true.
// The callback itself is covered at the walker level in
// tests/corpusfs/symlink_skip_781_test.go.

// symlinkCorpus builds a corpus of links and runs one ingest pass over it.
// It reports false when the filesystem cannot create a link, so a CI image
// without that permission skips instead of failing.
func symlinkCorpus(t *testing.T, followSymlinks bool) (*store.SQLiteStore, *ingest.Service, bool) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	// The link targets live OUTSIDE the corpus, which is the shape the issue
	// describes: a curated tree of links into a media library.
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "clip.txt"), []byte("real content"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(external, "season"), 0o700); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(external, "season", "ep1.txt"), []byte("episode"), 0o600); err != nil {
		t.Fatalf("write nested target: %v", err)
	}

	if err := os.Symlink(filepath.Join(external, "clip.txt"), filepath.Join(root, "clip.txt")); err != nil {
		t.Logf("this filesystem cannot create a symlink, so the case is skipped: %v", err)
		return nil, nil, false
	}
	if err := os.Symlink(filepath.Join(external, "season"), filepath.Join(root, "season")); err != nil {
		t.Logf("this filesystem cannot create a directory symlink, so the case is skipped: %v", err)
		return nil, nil, false
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	cfg.IngestFollowSymlinks = followSymlinks

	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st, svc, true
}

// TestSymlinkOnlyCorpusReportsItsSkips is the case from the issue. Before the
// count, such a corpus reported scanned=0, skipped=0, errors=0 and a ready
// daemon. That output is what an empty directory reports, and what a wrong
// root_dir reports, so an operator could not tell the three apart.
func TestSymlinkOnlyCorpusReportsItsSkips(t *testing.T) {
	st, _, ok := symlinkCorpus(t, false)
	if !ok {
		return
	}
	ctx := context.Background()

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil {
		t.Fatalf("SkipSummary = nil, want the symlink drops")
	}
	got := stats.SkipSummary.Categories[model.SkipReasonSymlinkIgnored]
	if got == 0 {
		t.Fatalf("a corpus of only symlinks reported no symlink_ignored skips: %+v", stats.SkipSummary.Categories)
	}
}

// TestADirectorySymlinkIsCountedToo: with following off the walker never
// resolves the target, so it cannot tell a link to a file from a link to a
// directory. Both must therefore be reported the same way, which is what the
// spec text for symlink_ignored says.
func TestADirectorySymlinkIsCountedToo(t *testing.T) {
	st, _, ok := symlinkCorpus(t, false)
	if !ok {
		return
	}
	ctx := context.Background()
	assertSkipReason(t, ctx, st, "season", "skipped", model.SkipReasonSymlinkIgnored)
	assertSkipReason(t, ctx, st, "clip.txt", "skipped", model.SkipReasonSymlinkIgnored)
}

// TestFollowSymlinksTrueCountsNoSkips is the other half of the contract. When
// the operator opts in, the links are followed and indexed, so there is
// nothing to report. A skip row here would mean the count fires on a path the
// walker actually took.
func TestFollowSymlinksTrueCountsNoSkips(t *testing.T) {
	st, _, ok := symlinkCorpus(t, true)
	if !ok {
		return
	}
	ctx := context.Background()

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary != nil {
		if got := stats.SkipSummary.Categories[model.SkipReasonSymlinkIgnored]; got != 0 {
			t.Fatalf("follow_symlinks=true recorded %d symlink_ignored skips, want 0", got)
		}
	}
}
