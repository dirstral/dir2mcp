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

// TestForegroundStartsOverRecycledPid pins the issue #665 fix: the
// single-instance lock that every `up` mode takes must use the same
// token-aware ownership check as `down`, `status` and `reindex`.
//
// A pid file left by a crash can name a pid the OS has since recycled to an
// unrelated process. A bare liveness check reads that as the live server, so
// `up --foreground` (the launchd/systemd service target) refuses to start and
// the operator is locked out of the corpus until the pid file is deleted by
// hand.
//
// The recycled pid is built deterministically instead of waiting for the OS to
// reuse one: the record names this live test process but carries a start-time
// token that does not match it, which is exactly what classifyPIDFile reports
// as pidRecycled.
func TestForegroundStartsOverRecycledPid(t *testing.T) {
	realToken := requireStartTokens(t)
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken+"-stale"); err != nil {
		t.Fatalf("seed recycled pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
		if code != 0 {
			t.Fatalf("foreground up refused to start over a recycled pid: code=%d stderr=%s", code, stderr.String())
		}
	})
	if strings.Contains(stderr.String(), "already running") {
		t.Fatalf("a recycled pid was mistaken for the live server: %s", stderr.String())
	}
}

// TestForegroundRefusesLiveOwnerWithMatchingToken is the counterpart guard: the
// token-aware reconciliation must not clear a pid file that still names our
// live server. The record here carries this process's real start-time token, so
// classifyPIDFile reports pidLive and the start must be refused with the pid
// file left intact (issue #665 must not weaken the #434 single-instance rule).
func TestForegroundRefusesLiveOwnerWithMatchingToken(t *testing.T) {
	realToken := requireStartTokens(t)
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken); err != nil {
		t.Fatalf("seed live pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(10*time.Second))
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
		if code == 0 {
			t.Fatalf("foreground up served a corpus a live owner holds; stderr=%s", stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("expected an 'already running' refusal, got stderr=%s", stderr.String())
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("a live owner's pid file must survive the refusal; stat err=%v", err)
	}
}
