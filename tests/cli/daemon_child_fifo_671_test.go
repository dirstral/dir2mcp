//go:build unix

package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestDaemonHandshakePathCannotHangStartup guards the handshake read (#671).
// The handshake path arrives in the environment, so it is untrusted. A plain
// open of a FIFO waits for a writer, so a marker plus a FIFO path would hang
// `dir2mcp up` before it starts. The read must not block, and the FIFO must
// not pass as a handshake, so the run takes the foreground path and creates
// <state_dir>/server.log.
func TestDaemonHandshakePathCannotHangStartup(t *testing.T) {
	tmp := t.TempDir()
	fifo := filepath.Join(tmp, "handshake.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	t.Setenv(daemonChildEnvName, "0123456789abcdef0123456789abcdef")
	t.Setenv(daemonHandshakeEnvName, fifo)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	done := make(chan int, 1)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		go func() { done <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"}) }()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("foreground up failed: code=%d stderr=%s", code, stderr.String())
			}
		case <-time.After(raceScaled(30 * time.Second)):
			// Deliberately bounded here rather than left to the package
			// timeout: a blocking read on the FIFO never returns.
			t.Fatal("startup blocked on the handshake path; the read must not wait for a FIFO writer")
		}
	})

	logPath := filepath.Join(tmp, ".dir2mcp", "server.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("a FIFO must not pass as a handshake: %s missing (%v)", logPath, err)
	}
}
