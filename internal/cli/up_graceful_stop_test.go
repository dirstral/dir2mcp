package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUp_GracefulCancelExitsZero pins the #434 precondition the launchd
// KeepAlive{SuccessfulExit=false} and systemd Restart=on-failure contracts
// depend on: a SIGTERM-triggered graceful stop of `up --foreground` (the
// supervised service target) must exit 0, not the interrupt code — otherwise
// a `dir2mcp down` (which SIGTERMs the daemon) would look like a crash and the
// supervisor would respawn it.
//
// The signal is modeled by cancelling the run context (exactly what
// signal.NotifyContext does on SIGTERM). The test asserts both halves of the
// chain: runUp marks the stop graceful, and resolveProcessExitCode then keeps
// the exit at 0.
func TestUp_GracefulCancelExitsZero(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	// Skip the network embed-preflight probe so the daemon reaches the serving
	// loop without valid live credentials (mirrors the tests/cli TestMain).
	t.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")
	t.Chdir(tmp)

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	app := NewAppWithIO(io.Discard, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
	}()

	// Wait until the daemon is serving (connection.json published), then stop
	// it the way a signal would.
	connPath := connectionFilePath(stateDir)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(connPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-codeCh
			t.Fatal("daemon never became ready (connection.json not written)")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	var code int
	select {
	case code = <-codeCh:
	case <-time.After(15 * time.Second):
		t.Fatal("up did not exit after context cancellation")
	}

	if code != exitSuccess {
		t.Fatalf("graceful stop returned code %d from the serve loop, want %d", code, exitSuccess)
	}
	if !app.serverGracefulStop {
		t.Fatal("serverGracefulStop not set; a signal stop would be misread as an interrupt")
	}
	// The Run() wrapper maps a cancelled clean exit; with the graceful flag it
	// must stay 0 so the supervisor does not treat `down` as a crash.
	if got := resolveProcessExitCode(context.Canceled, code, app.serverGracefulStop); got != exitSuccess {
		t.Fatalf("graceful SIGTERM exit code = %d, want %d", got, exitSuccess)
	}
}
