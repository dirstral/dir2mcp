package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// daemonChildEnvName is the marker the daemon parent sets on the child it
// spawns. The tests below drive it from the outside, the way a leaked shell
// export or a CI runner variable would.
const daemonChildEnvName = "DIR2MCP_DAEMON_CHILD"

// daemonHandshakeEnvName carries the path of the file that holds the same
// nonce as the marker. Both halves must agree for a process to hold the
// daemon-child role.
const daemonHandshakeEnvName = "DIR2MCP_DAEMON_HANDSHAKE"

// TestForegroundIgnoresUnverifiedDaemonMarker pins the issue #671 fix: the
// daemon-child role was granted on marker length alone, so any value of 16 or
// more characters turned a plain `dir2mcp up` into a process that behaves like
// the daemon body. The operator gets a foreground process that prints no
// banner, does not answer q+Enter, and writes no <state_dir>/server.log,
// because the log tee is skipped for a child whose stderr the parent has
// already pointed at that file.
//
// server.log is the observable: the foreground tee creates it, and the daemon
// child never does. A run that produces the file took the foreground path.
func TestForegroundIgnoresUnverifiedDaemonMarker(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	// A marker of exactly the real nonce length, with no handshake behind it.
	// Length must not be evidence.
	t.Setenv(daemonChildEnvName, strings.Repeat("a", 32))
	t.Setenv(daemonHandshakeEnvName, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		if code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("foreground up failed: code=%d stderr=%s", code, stderr.String())
		}
	})

	logPath := filepath.Join(tmp, ".dir2mcp", "server.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("an unverified marker selected daemon-child behaviour: %s missing (%v); stderr=%s",
			logPath, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), daemonChildEnvName) {
		t.Errorf("an unverified marker should be reported to the operator; stderr=%s", stderr.String())
	}
}

// TestForegroundHonoursVerifiedDaemonHandshake is the counterpart: a process
// that holds a genuine handshake, the one the daemon parent prepares before it
// spawns the child, must still take the daemon-child path. The handshake is
// built with the production helper, so the test cannot pass by accident.
//
// The daemon child skips the server.log tee, so the absence of the file is the
// evidence here.
func TestForegroundHonoursVerifiedDaemonHandshake(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	childEnv, cleanup, err := cli.DaemonChildHandshakeEnvForTest(stateDir)
	if err != nil {
		t.Fatalf("prepare daemon handshake: %v", err)
	}
	defer cleanup()
	for _, entry := range childEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed child env entry %q", entry)
		}
		t.Setenv(name, value)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		if code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("foreground up failed: code=%d stderr=%s", code, stderr.String())
		}
	})

	logPath := filepath.Join(stateDir, "server.log")
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("a verified daemon child must not tee %s (its stderr is already that file)", logPath)
	}
	if strings.Contains(stderr.String(), "did not verify") {
		t.Errorf("a genuine handshake must not warn; stderr=%s", stderr.String())
	}
}

// TestDaemonHandshakeIsSingleUse verifies the nonce admits one process once.
// The first verified run consumes the handshake file, so a second process that
// inherits the same environment (a grandchild, or a replay) is not the daemon
// child any more. Before the fix the marker was reusable for ever, because
// nothing was consumed and nothing was compared.
func TestDaemonHandshakeIsSingleUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	childEnv, cleanup, err := cli.DaemonChildHandshakeEnvForTest(stateDir)
	if err != nil {
		t.Fatalf("prepare daemon handshake: %v", err)
	}
	defer cleanup()
	var handshakePath string
	for _, entry := range childEnv {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
		if name == daemonHandshakeEnvName {
			handshakePath = value
		}
	}

	// First run: a real daemon child, which consumes the handshake.
	firstApp := cli.NewAppWithIO(&bytes.Buffer{}, &bytes.Buffer{})
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		if code := firstApp.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("first foreground up failed")
		}
	})
	if _, err := os.Stat(handshakePath); err == nil {
		t.Fatal("a verified handshake must be consumed")
	}

	// Second run with the very same environment: the role is gone.
	var stderr bytes.Buffer
	secondApp := cli.NewAppWithIO(&bytes.Buffer{}, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		if code := secondApp.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("second foreground up failed: stderr=%s", stderr.String())
		}
	})
	if _, err := os.Stat(filepath.Join(stateDir, "server.log")); err != nil {
		t.Fatalf("a replayed marker must fall back to the foreground path: %v; stderr=%s", err, stderr.String())
	}
}
