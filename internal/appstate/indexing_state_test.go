package appstate_test

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
)

// TestWatchOverflowsCounter covers the issue #409 item 5 observability metric:
// AddWatchOverflows accumulates into the snapshot, and — unlike the per-scan
// progress counters — it survives ResetProgress, since it reflects
// watcher-lifetime event drops rather than a single scan's work.
func TestWatchOverflowsCounter(t *testing.T) {
	s := appstate.NewIndexingState(appstate.ModeIncremental)

	if got := s.Snapshot().WatchOverflows; got != 0 {
		t.Fatalf("initial WatchOverflows = %d, want 0", got)
	}

	s.AddWatchOverflows(1)
	s.AddWatchOverflows(2)
	if got := s.Snapshot().WatchOverflows; got != 3 {
		t.Fatalf("WatchOverflows after adds = %d, want 3", got)
	}

	// Progress reset must NOT clear a watcher-lifetime counter (mirrors embeddedOK).
	s.AddScanned(5)
	s.ResetProgress()
	snap := s.Snapshot()
	if snap.Scanned != 0 {
		t.Fatalf("Scanned after ResetProgress = %d, want 0", snap.Scanned)
	}
	if snap.WatchOverflows != 3 {
		t.Fatalf("WatchOverflows after ResetProgress = %d, want 3 (must survive reset)", snap.WatchOverflows)
	}

	// A nil receiver is a safe no-op (mirrors the other Add* methods).
	var nilState *appstate.IndexingState
	nilState.AddWatchOverflows(1)
}
