package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// requireStartTokens skips a test when the platform can't read a process
// start-time token. Without it, pid-reuse protection (issue #418) degrades to
// a bare liveness check and the recycled-pid branch these tests exercise is
// unreachable, so the assertions would not hold.
func requireStartTokens(t *testing.T) string {
	t.Helper()
	tok, ok := cli.ProcessStartTokenForTest(os.Getpid())
	if !ok {
		t.Skip("process start-time token unavailable on this platform; pid-reuse protection inactive")
	}
	return tok
}

// TestDown_RecycledPid_LeavesProcessUntouched verifies the issue #418 fix for
// `down`: after a crash without cleanup the OS can recycle the recorded pid to
// an unrelated process. `down` must recognise the mismatched start-time token,
// report "recycled_pid", signal NOTHING, and clear the stale pid file — rather
// than SIGTERM/SIGKILL a bystander. (If the fix regressed, `down` would call
// stopDaemon(os.Getpid()) and kill this very test process instead of returning.)
func TestDown_RecycledPid_LeavesProcessUntouched(t *testing.T) {
	realToken := requireStartTokens(t)
	root, stateDir := newDownFixture(t)
	pidPath := filepath.Join(stateDir, "server.pid")
	// A live pid (this test process) but a start-time token that deliberately
	// does not match its real one — the signature of a recycled pid.
	if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken+"-stale"); err != nil {
		t.Fatalf("seed recycled pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, root, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "down"})
		if code != 0 {
			t.Fatalf("exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Pid     int    `json:"pid"`
		Stopped bool   `json:"stopped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal stdout: %v raw=%s", err, stdout.String())
	}
	if payload.Reason != "recycled_pid" {
		t.Errorf("reason: want recycled_pid got %q", payload.Reason)
	}
	if payload.Stopped {
		t.Error("stopped: want false — down must not signal a recycled pid")
	}
	if payload.Pid != os.Getpid() {
		t.Errorf("pid: want %d got %d", os.Getpid(), payload.Pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale pid file should have been removed; stat err=%v", err)
	}
}

// TestReindex_ProceedsWhenPidRecycled verifies the issue #418 fix for the
// reindex guard: a recycled pid (alive, but not our daemon) must NOT block a
// reindex. Only a genuinely live daemon — one whose recorded start-time token
// matches — holds the index open and warrants refusal.
func TestReindex_ProceedsWhenPidRecycled(t *testing.T) {
	realToken := requireStartTokens(t)
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken+"-stale"); err != nil {
		t.Fatalf("seed recycled pid file: %v", err)
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
	if code != 0 {
		t.Fatalf("reindex should proceed past a recycled pid; exit=%d stderr=%q", code, stderr.String())
	}
	if !reindexCalled {
		t.Error("ingestor should be constructed when reindex proceeds past a recycled pid")
	}
}

// TestUp_RefusesWhenOurDaemonLive verifies the counterpart to the recycled-pid
// case: `up --daemon` must still refuse to start a second daemon when a live
// daemon that is genuinely OURS (matching start-time token) already owns the
// state dir, and must not clear its pid file (issue #418). This exercises the
// identity-verified "already running" refusal without spawning a child (the
// refusal returns before the fork).
func TestUp_RefusesWhenOurDaemonLive(t *testing.T) {
	realToken := requireStartTokens(t)
	tmp := t.TempDir()
	// A non-empty key so the parent's config preflight passes and reaches the
	// pid-file ownership check (the check runs after preflight, before fork).
	t.Setenv("MISTRAL_API_KEY", "test-key-not-used")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	pidPath := filepath.Join(stateDir, "server.pid")
	// This process's pid WITH its real token is, by the identity check, our own
	// live daemon.
	if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken); err != nil {
		t.Fatalf("seed live pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"up", "--daemon", "--listen", "127.0.0.1:0"})
	})
	if code == 0 {
		t.Fatalf("up should refuse when our daemon is already live; got exit 0 stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "already running") {
		t.Fatalf("stderr should report the daemon is already running; got %q", got)
	}
	// up must not clear a genuinely live daemon's registration.
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("live pid file must not be removed; stat err=%v", err)
	}
}
