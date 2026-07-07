package tests

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestFileCapOversize_SurfacedAsSkippedAndLogged is the regression guard for
// issue #497: a file larger than the ingest file-size cap is dropped at
// discovery. Previously that drop was silent — the file was not counted as
// skipped and no reason was recorded, so `status` showed skipped=0 and the
// operator had no signal that (e.g.) a large media file had been excluded.
//
// The exclusion must now be observable: the over-cap file is counted in the run
// counters (scanned+skipped) and a discovery log line names the ingest.max_file_mb
// cap so it is actionable rather than a silent no-op.
func TestFileCapOversize_SurfacedAsSkippedAndLogged(t *testing.T) {
	// getLogger falls back to log.Default() when no per-service logger is set, so
	// redirect the process logger to observe the discovery line. Mutates global
	// state; must not be parallelised. Restored via t.Cleanup.
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	ctx := context.Background()
	root := t.TempDir()

	// A tiny file that fits, and one that exceeds a 1 MiB cap.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("small and fine"), 0o600); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	oversize := bytes.Repeat([]byte("x"), 2*1024*1024) // 2 MiB > 1 MiB cap
	if err := os.WriteFile(filepath.Join(root, "big.mp4"), oversize, 0o600); err != nil {
		t.Fatalf("write oversize file: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = 1 // 1 MiB cap; the 2 MiB file must be excluded.

	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := state.Snapshot()
	if snap.Skipped < 1 {
		t.Errorf("snapshot.Skipped = %d, want >= 1 (the over-cap file must be counted as skipped, not dropped silently)", snap.Skipped)
	}
	// The over-cap file must be accounted for in scanned too, so it is not an
	// invisible exclusion missing from the corpus totals.
	if snap.Scanned < 2 {
		t.Errorf("snapshot.Scanned = %d, want >= 2 (the over-cap file must be counted as scanned)", snap.Scanned)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "big.mp4") {
		t.Errorf("discovery log did not mention the excluded file; got: %q", logged)
	}
	if !strings.Contains(logged, "ingest.max_file_mb") {
		t.Errorf("discovery log did not name the ingest.max_file_mb cap; got: %q", logged)
	}
}
