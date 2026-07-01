package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// notChunkSourceStore is a minimal model.Store that deliberately does NOT
// implement index.ChunkSource, used to exercise the assertion-failure path in
// startEmbeddingIfNotReadOnly (issue #374). The shipped *store.SQLiteStore
// always satisfies ChunkSource (guarded at compile time), so a fake is the only
// way to drive the "embedding disabled" warning branch.
type notChunkSourceStore struct{}

func (notChunkSourceStore) Init(context.Context) error { return nil }
func (notChunkSourceStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (notChunkSourceStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, nil
}
func (notChunkSourceStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (notChunkSourceStore) Close() error { return nil }

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

	t.Run("ephemeral with no prior connection uses a deterministic per-corpus port", func(t *testing.T) {
		// No connection.json (fresh corpus, or after down/rm) must NOT drift to a
		// fresh random port — it should bind a stable port derived from the state
		// dir so the baked client URL keeps working across reinstalls (#386).
		stateDir := t.TempDir()
		want := net.JoinHostPort("127.0.0.1", deterministicPort(stateDir))
		if got := preferredListenAddr("127.0.0.1:0", stateDir); got != want {
			t.Fatalf("want deterministic %q, got %q", want, got)
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

// TestDeterministicPort covers the per-corpus port derivation (#386): stable for
// a given state dir, inside the IANA dynamic range, and corpus-specific so two
// corpora almost never collide.
func TestDeterministicPort(t *testing.T) {
	a := t.TempDir()
	p := deterministicPort(a)
	if p == "" || p != deterministicPort(a) {
		t.Fatalf("deterministicPort not stable for %q: %q", a, p)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 49152 || n > 65535 {
		t.Fatalf("port %q outside dynamic range 49152-65535", p)
	}
	if other := deterministicPort(t.TempDir()); other == p {
		t.Errorf("distinct corpora collided on port %q", p)
	}
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

// TestPickEmbedLogger_JSONModeReachesServerLog verifies that in JSON/daemon mode
// the embed-worker logger is NOT discarded (issue #374): it writes to the
// process-global log destination, which teeServerLog has wired to server.log. A
// discarded logger would make the issue-#364 "embed worker started" diagnostic —
// whose absence is the diagnosis — impossible to ever observe in a real daemon.
func TestPickEmbedLogger_JSONModeReachesServerLog(t *testing.T) {
	t.Setenv(daemonChildEnv, "") // foreground tee path (daemon child wires stderr→file at the OS level)
	stateDir := t.TempDir()
	app := NewAppWithIO(io.Discard, &bytes.Buffer{})

	restore := app.teeServerLog(stateDir)
	if restore == nil {
		t.Fatal("expected tee to be active so log.Writer() reaches server.log")
	}
	defer restore()

	// stderr is intentionally a buffer we ignore: in JSON mode the embed logger
	// must route to log.Writer() (→ server.log), not to stderr.
	embedLogger := pickEmbedLogger(io.Discard, true /* jsonOutput */)
	const marker = "embed worker started [kind=text]"
	embedLogger.Printf("%s", marker)

	data, err := os.ReadFile(serverLogPath(stateDir))
	if err != nil {
		t.Fatalf("read server.log: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Fatalf("JSON-mode embed logger output missing from server.log (was it discarded?); got %q", data)
	}
}

// TestStartEmbeddingIfNotReadOnly_NoChunkSourceLogsWarning verifies that when the
// store does not satisfy index.ChunkSource, embedding is not silently disabled
// (issue #374): a warning is written to server.log (via the embed logger →
// log.Writer() tee) AND emitted as a structured NDJSON event.
func TestStartEmbeddingIfNotReadOnly_NoChunkSourceLogsWarning(t *testing.T) {
	t.Setenv(daemonChildEnv, "")
	stateDir := t.TempDir()
	var stdout bytes.Buffer
	app := NewAppWithIO(&stdout, io.Discard)

	restore := app.teeServerLog(stateDir)
	if restore == nil {
		t.Fatal("expected tee to be active so the embed logger reaches server.log")
	}
	defer restore()

	emitter := newNDJSONEmitter(&stdout, true /* enabled */)

	err := startEmbeddingIfNotReadOnly(
		context.Background(),
		config.Config{},
		false, // readOnly: must be false to reach the ChunkSource assertion
		notChunkSourceStore{},
		nil, nil, // textIx, codeIx
		nil,                 // embedder
		nil,                 // ret
		nil,                 // indexingState
		make(chan error, 1), // embedErrCh
		io.Discard,          // stderr
		true,                // jsonOutput
		"", "", "",          // embedModelText, embedModelCode, rootDir
		nil, // corpusFS
		emitter,
	)
	if err != nil {
		t.Fatalf("startEmbeddingIfNotReadOnly returned error: %v", err)
	}

	// server.log got the human warning line.
	logData, readErr := os.ReadFile(serverLogPath(stateDir))
	if readErr != nil {
		t.Fatalf("read server.log: %v", readErr)
	}
	if !strings.Contains(string(logData), "does not satisfy index.ChunkSource") {
		t.Fatalf("server.log missing ChunkSource-disabled warning; got %q", logData)
	}

	// stdout got the structured NDJSON warning event.
	if !strings.Contains(stdout.String(), "embedding_disabled_no_chunk_source") {
		t.Fatalf("NDJSON warning event missing from stdout; got %q", stdout.String())
	}
}
