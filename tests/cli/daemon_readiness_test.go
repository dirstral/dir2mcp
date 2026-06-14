//go:build unix

package tests

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestDaemonReadinessTimeoutExceedsDoclingProbe guards the ordering invariant
// behind the first-run UX fix: the daemon readiness window MUST stay above the
// document-extractor functional-probe ceiling. On dir2mcp-full a cold first run
// runs the docling probe (which imports torch) BEFORE the child binds, so a
// readiness window at or below the probe ceiling would trip a false "not ready"
// even though the daemon comes up fine. If anyone raises the probe timeout
// without raising the readiness window, this test fails before the regression
// can ship.
func TestDaemonReadinessTimeoutExceedsDoclingProbe(t *testing.T) {
	readiness := cli.DaemonReadinessTimeout()
	probe := ingest.DoclingProbeTimeout()
	if readiness <= probe {
		t.Fatalf("daemon readiness timeout (%s) must exceed docling probe ceiling (%s); "+
			"a cold dir2mcp-full first run probes the extractor before binding, so the "+
			"readiness window has to clear the probe with headroom", readiness, probe)
	}
	// Sanity-check there is meaningful headroom (index load + bind), not just a
	// hair above the probe.
	if readiness < probe+5*time.Second {
		t.Errorf("daemon readiness timeout (%s) should leave headroom over the probe ceiling (%s) "+
			"for index load and listener bind", readiness, probe)
	}
}

// TestDaemonStillStartingClassification covers the three outcomes
// waitForConnectionFile produces, via the exported WaitForConnectionReady
// wrapper, without spinning up a real daemon:
//
//   - ready: connection.json with a URL is present -> success, no error
//   - still-starting: child alive but no connection.json within the window ->
//     errDaemonStillStarting (the healthy-but-slow first-run case)
//   - crashed: child pid no longer alive -> a hard error that is NOT classified
//     as still-starting
func TestDaemonStillStartingClassification(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		dir := t.TempDir()
		connPath := filepath.Join(dir, "connection.json")
		if err := os.WriteFile(connPath, []byte(`{"url":"http://127.0.0.1:9999/mcp"}`), 0o600); err != nil {
			t.Fatalf("write connection.json: %v", err)
		}
		ready, err := cli.WaitForConnectionReady(connPath, os.Getpid(), 2*time.Second)
		if err != nil {
			t.Fatalf("expected ready, got error: %v", err)
		}
		if !ready {
			t.Fatalf("expected ready=true")
		}
		if cli.IsDaemonStillStarting(err) {
			t.Errorf("ready result must not classify as still-starting")
		}
	})

	t.Run("still starting", func(t *testing.T) {
		dir := t.TempDir()
		connPath := filepath.Join(dir, "connection.json") // never written
		// os.Getpid() is alive for the duration of the test, so the deadline
		// expires while the "child" is still alive -> still-starting.
		ready, err := cli.WaitForConnectionReady(connPath, os.Getpid(), 250*time.Millisecond)
		if ready {
			t.Fatalf("expected not ready when connection.json never appears")
		}
		if err == nil {
			t.Fatalf("expected an error on timeout")
		}
		if !cli.IsDaemonStillStarting(err) {
			t.Fatalf("alive-but-slow child should classify as still-starting; got: %v", err)
		}
	})

	t.Run("crashed", func(t *testing.T) {
		dir := t.TempDir()
		connPath := filepath.Join(dir, "connection.json") // never written
		deadPid := spawnAndReapForDeadPID(t)
		ready, err := cli.WaitForConnectionReady(connPath, deadPid, 5*time.Second)
		if ready {
			t.Fatalf("expected not ready for a dead child pid")
		}
		if err == nil {
			t.Fatalf("expected an error for a dead child pid")
		}
		if cli.IsDaemonStillStarting(err) {
			t.Errorf("a crashed child must NOT classify as still-starting (it is a real failure); got: %v", err)
		}
	})
}

// spawnAndReapForDeadPID starts a trivial child, waits for it to exit and be
// reaped, and returns its now-dead pid. This gives the classification test a
// pid that processIsAlive will report as not-alive without guessing an
// arbitrary integer that might collide with a live process.
func spawnAndReapForDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		// "true" exits 0; any error here means the process didn't run as expected.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait throwaway process: %v", err)
		}
	}
	return pid
}
