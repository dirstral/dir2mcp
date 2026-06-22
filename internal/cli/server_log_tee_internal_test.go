package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreferredListenAddr verifies the sticky-port logic (#368): the ephemeral
// default (host:0) is replaced with a prior run's recorded port so a restart
// re-binds the same port and the baked-in Claude URL keeps working; an explicit
// port is respected; and the ephemeral default is kept when nothing is recorded.
func TestPreferredListenAddr(t *testing.T) {
	t.Run("explicit port respected", func(t *testing.T) {
		got := preferredListenAddr("127.0.0.1:8080", t.TempDir())
		if got != "127.0.0.1:8080" {
			t.Fatalf("explicit port should be respected, got %q", got)
		}
	})

	t.Run("ephemeral with no prior connection stays ephemeral", func(t *testing.T) {
		got := preferredListenAddr("127.0.0.1:0", t.TempDir())
		if got != "127.0.0.1:0" {
			t.Fatalf("want ephemeral 127.0.0.1:0 with no prior port, got %q", got)
		}
	})

	t.Run("ephemeral reuses prior recorded port", func(t *testing.T) {
		stateDir := t.TempDir()
		raw, err := json.Marshal(connectionPayload{URL: "http://127.0.0.1:58210/mcp"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, connectionFileName), raw, 0o600); err != nil {
			t.Fatalf("write connection.json: %v", err)
		}
		got := preferredListenAddr("127.0.0.1:0", stateDir)
		if got != "127.0.0.1:58210" {
			t.Fatalf("want sticky 127.0.0.1:58210 from prior connection.json, got %q", got)
		}
	})
}

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
