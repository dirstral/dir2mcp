package tests

import (
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
)

// TestResetProgress_ZeroesRunCountersPreservesEmbedded verifies that
// ResetProgress clears the per-scan progress counters (issue #426) while
// leaving embeddedOK — owned by the separately-running, resumable embed worker
// and preloaded from the store — intact, and without disturbing the
// jobID/mode/running lifecycle fields.
func TestResetProgress_ZeroesRunCountersPreservesEmbedded(t *testing.T) {
	s := appstate.NewIndexingState(appstate.ModeFull)
	s.SetJobID("job_fixed")
	s.SetRunning(true)

	s.AddScanned(7)
	s.AddIndexed(4)
	s.AddSkipped(2)
	s.AddDeleted(1)
	s.AddRepresentations(9)
	s.AddChunksTotal(11)
	s.AddErrors(3)
	s.AddEmbeddedOK(5)

	s.ResetProgress()

	got := s.Snapshot()
	if got.Scanned != 0 || got.Indexed != 0 || got.Skipped != 0 || got.Deleted != 0 ||
		got.Representations != 0 || got.ChunksTotal != 0 || got.Errors != 0 {
		t.Fatalf("ResetProgress left run counters non-zero: %+v", got)
	}
	if got.EmbeddedOK != 5 {
		t.Fatalf("ResetProgress must preserve EmbeddedOK: got %d want 5", got.EmbeddedOK)
	}
	if got.JobID != "job_fixed" {
		t.Fatalf("ResetProgress must preserve JobID: got %q", got.JobID)
	}
	if got.Mode != appstate.ModeFull {
		t.Fatalf("ResetProgress must preserve Mode: got %q", got.Mode)
	}
	if !got.Running {
		t.Fatal("ResetProgress must preserve Running=true")
	}
}

// TestResetProgress_SnapshotInvariantDuringConcurrentReset guards the counter
// ordering in ResetProgress: because Snapshot() reads each field independently
// without a lock, a status scrape can interleave with a reset. Zeroing the
// component counters before scanned keeps the indexed+skipped+errors <= scanned
// invariant true for every observer, so a snapshot taken mid-reset never sees
// scanned=0 while indexed/skipped/errors still carry the previous run's totals.
// Run with -race to also surface any unsynchronised access.
func TestResetProgress_SnapshotInvariantDuringConcurrentReset(t *testing.T) {
	s := appstate.NewIndexingState(appstate.ModeFull)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				got := s.Snapshot()
				if got.Indexed+got.Skipped+got.Errors > got.Scanned {
					t.Errorf("invariant violated mid-reset: indexed(%d)+skipped(%d)+errors(%d) > scanned(%d)",
						got.Indexed, got.Skipped, got.Errors, got.Scanned)
					return
				}
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		s.AddScanned(10)
		s.AddIndexed(4)
		s.AddSkipped(3)
		s.AddErrors(2)
		s.ResetProgress()
	}

	close(stop)
	wg.Wait()
}

// TestResetProgress_NilReceiverIsSafe mirrors the nil-guard pattern used by the
// other IndexingState mutators so a scan on a service with no wired-up state
// does not panic.
func TestResetProgress_NilReceiverIsSafe(t *testing.T) {
	var s *appstate.IndexingState
	s.ResetProgress()
}
