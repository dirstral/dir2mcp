package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestReindex_RefusesWhenDaemonAlive verifies the issue #418 guard:
// `reindex` must refuse — with a clear, actionable error and a non-zero
// exit — when a live daemon owns the same state dir, and it must NOT
// delete the on-disk index files before bailing out. Otherwise reindex
// runs concurrent sqlite writers and unlinks index files the daemon
// still holds open, corrupting the shared state.
func TestReindex_RefusesWhenDaemonAlive(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// A pid file naming THIS (alive) process stands in for a live daemon.
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	// A sentinel index file that prepareReindexStore would remove if the
	// guard failed to fire.
	indexFile := filepath.Join(stateDir, "vectors_text.v2.hnsw")
	if err := os.WriteFile(indexFile, []byte("INDEX"), 0o600); err != nil {
		t.Fatalf("seed index file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	reindexCalled := false
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			reindexCalled = true
			return reindexNoopIngestor{}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})

	if code == 0 {
		t.Fatalf("expected non-zero exit when daemon is alive; got 0 (stderr=%q)", stderr.String())
	}
	if reindexCalled {
		t.Error("ingestor should not be constructed when reindex refuses")
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "dir2mcp down") {
		t.Errorf("error should tell the user to stop the daemon with `dir2mcp down`; got %q", errOut)
	}
	if _, err := os.Stat(indexFile); err != nil {
		t.Errorf("index file must NOT be deleted when reindex refuses; stat err=%v", err)
	}
	if data, err := os.ReadFile(indexFile); err != nil || string(data) != "INDEX" {
		t.Errorf("index file content changed; data=%q err=%v", string(data), err)
	}
}

// TestReindex_ProceedsWithStalePidFile verifies the guard does not
// false-positive on a stale pid file (a daemon that crashed without
// cleanup): a pid that is not alive must let the reindex run.
func TestReindex_ProceedsWithStalePidFile(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// A high pid that is almost certainly dead exercises the
	// processIsAlive==false branch (stale pid file from a crashed daemon).
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := os.WriteFile(pidPath, []byte("2147480000\n"), 0o600); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			return reindexNoopIngestor{}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})

	if code != 0 {
		t.Fatalf("reindex should proceed with a stale pid file; exit=%d stderr=%q", code, stderr.String())
	}
}
