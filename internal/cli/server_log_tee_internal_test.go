package cli

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// TestTeeServerLog_WritesToServerLogInForeground verifies that a foreground /
// service launch (not the daemon child) mirrors the process logger to
// <state_dir>/server.log, and that restore() bounds the global-logger mutation
// so it never leaks past the server's lifetime. Regression for issue #360.
func TestTeeServerLog_WritesToServerLogInForeground(t *testing.T) {
	t.Setenv(daemonChildEnv, "") // ensure not treated as a daemon child
	stateDir := t.TempDir()
	app := NewAppWithIO(io.Discard, &bytes.Buffer{})

	restore := app.teeServerLog(stateDir)
	if restore == nil {
		t.Fatal("expected tee to be active in a non-daemon-child (foreground) run")
	}
	const marker = "diag-marker-360-foreground"
	log.Printf("%s", marker)
	restore()

	data, err := os.ReadFile(serverLogPath(stateDir))
	if err != nil {
		t.Fatalf("read server.log: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Fatalf("server.log missing the logged marker; got %q", data)
	}

	// After restore the logger must no longer write to the (now closed) file.
	log.Printf("post-restore-should-not-appear")
	after, err := os.ReadFile(serverLogPath(stateDir))
	if err != nil {
		t.Fatalf("re-read server.log: %v", err)
	}
	if strings.Contains(string(after), "post-restore-should-not-appear") {
		t.Fatal("logger still writing to server.log after restore() — global mutation leaked")
	}
}

// TestTeeServerLog_NoopInDaemonChild verifies the tee is a no-op in the daemon
// child, whose stderr the parent already redirects to server.log — teeing again
// would double-write every line.
func TestTeeServerLog_NoopInDaemonChild(t *testing.T) {
	t.Setenv(daemonChildEnv, strings.Repeat("a", 64)) // looks like a daemon child
	app := NewAppWithIO(io.Discard, &bytes.Buffer{})
	if restore := app.teeServerLog(t.TempDir()); restore != nil {
		restore()
		t.Fatal("teeServerLog must be a no-op in the daemon child (stderr already → server.log)")
	}
}
