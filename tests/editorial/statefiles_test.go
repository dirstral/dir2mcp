package tests

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// Staged imports are applied in staging order (SPEC §7.11.1), also when the
// clock goes back or two imports share one timestamp.
func TestStage_KeepsStagingOrder(t *testing.T) {
	stateDir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	var paths []string
	for _, at := range []time.Time{now, now, now.Add(-time.Hour)} {
		p, err := editorial.Stage(stateDir, editorial.Staged{Format: 1, Source: "s", Key: editorial.KeyStem}, at)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		paths = append(paths, p)
	}
	listed, err := editorial.ListStaged(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d staged imports, want 3", len(listed))
	}
	for i := range paths {
		if filepath.Base(listed[i]) != filepath.Base(paths[i]) {
			t.Fatalf("staging order lost: staged %v, listed %v", paths, listed)
		}
	}
}

func TestRebuildMarker(t *testing.T) {
	stateDir := t.TempDir()
	if editorial.RebuildPending(stateDir) {
		t.Fatal("a fresh state dir reports a pending rebuild")
	}
	if err := editorial.MarkRebuildPending(stateDir); err != nil {
		t.Fatal(err)
	}
	if !editorial.RebuildPending(stateDir) {
		t.Fatal("the marker was not recorded")
	}
	if err := editorial.ClearRebuildPending(stateDir); err != nil {
		t.Fatal(err)
	}
	if editorial.RebuildPending(stateDir) {
		t.Fatal("the marker was not cleared")
	}
}
