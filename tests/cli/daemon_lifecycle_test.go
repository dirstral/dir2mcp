//go:build unix

package tests

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDaemonLifecycle_UpForksThenDownStops drives the full fork → poll →
// stop loop end-to-end against a freshly built dir2mcp binary. Gated under
// RUN_INTEGRATION_TESTS=1 because it shells out, binds a TCP port, and
// shells back through the OS process group machinery — none of which fits
// the unit-test budget. Set the env to run.
//
// The test does NOT need Mistral credentials: dir2mcp.up only requires a
// valid Mistral key for OCR/embedding paths, and on a fresh empty corpus
// the embed worker never gets called before we shut down.
func TestDaemonLifecycle_UpForksThenDownStops(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to exercise the daemon lifecycle")
	}
	if runtime.GOOS == "windows" {
		t.Skip("daemon mode is unix-only")
	}

	bin := buildDir2mcpBinary(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	logPath := filepath.Join(stateDir, "server.log")
	pidPath := filepath.Join(stateDir, "server.pid")
	connPath := filepath.Join(stateDir, "connection.json")

	env := append(os.Environ(),
		"MISTRAL_API_KEY=test-key", // empty corpus → key is never exercised
		"DIR2MCP_AUTH_TOKEN=",      // auto-generates a token
	)

	// `up` should fork a daemon and return promptly (well under our 30s
	// readiness timeout). We give it a generous wall-clock so a slow CI
	// runner doesn't false-fail.
	// --daemon forces daemon mode despite stdout being a pipe (tests
	// don't get a TTY). Without it, exec'd `up` defaults to foreground
	// and the test would never test what it claims to test.
	upCmd := exec.Command(bin, "up", "--daemon", "--listen", "127.0.0.1:0")
	upCmd.Dir = root
	upCmd.Env = env
	upOut, err := runWithDeadline(upCmd, 30*time.Second)
	if err != nil {
		t.Fatalf("dir2mcp up failed: %v\n%s", err, upOut)
	}
	if !strings.Contains(upOut, "daemon") {
		t.Errorf("up stdout should mention daemon mode, got: %s", upOut)
	}

	// Pid file should exist and point at a live process.
	pidRaw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil {
		t.Fatalf("parse pid file %q: %v", string(pidRaw), err)
	}
	if !pidAlive(pid) {
		t.Fatalf("daemon pid %d not alive after up", pid)
	}
	if _, err := os.Stat(connPath); err != nil {
		t.Errorf("connection.json missing: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("server.log missing: %v", err)
	}

	// `down` should signal the daemon and clean up the pid file.
	downCmd := exec.Command(bin, "down")
	downCmd.Dir = root
	downCmd.Env = env
	downOut, err := runWithDeadline(downCmd, 15*time.Second)
	if err != nil {
		t.Fatalf("dir2mcp down failed: %v\n%s", err, downOut)
	}
	if !strings.Contains(downOut, "stopped") {
		t.Errorf("down stdout should report stopped, got: %s", downOut)
	}
	// Process should be gone within the daemonShutdownGrace window.
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Errorf("daemon pid %d still alive after down", pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file should be removed after down; stat err=%v", err)
	}

	// Re-running down on a clean state must be a no-op.
	downAgain := exec.Command(bin, "down")
	downAgain.Dir = root
	downAgain.Env = env
	out2, err := runWithDeadline(downAgain, 5*time.Second)
	if err != nil {
		t.Fatalf("second dir2mcp down failed: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "no dir2mcp daemon registered") {
		t.Errorf("second down should report no-daemon, got: %s", out2)
	}
}

func buildDir2mcpBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dir2mcp")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/dir2mcp")
	cmd.Dir = repoRootForTest(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build dir2mcp: %v\n%s", err, out)
	}
	return bin
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	// tests/cli is two levels below the repo root.
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func runWithDeadline(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd2 := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd2.Dir = cmd.Dir
	cmd2.Env = cmd.Env
	out, err := cmd2.CombinedOutput()
	return string(out), err
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
