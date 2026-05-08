package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonChildEnv carries a per-spawn nonce that the parent sets on the
// child's environment. The child treats itself as a daemon body only if
// the env value is non-empty AND matches the value the parent's process
// memory still holds, so a manually-exported `DIR2MCP_DAEMON_CHILD=1`
// in a user's shell can't accidentally trip the daemon-child code paths
// (which would silently drop the banner, the stdin quit listener, and
// the parent's pid-file write — confusing footgun, no security boundary
// crossed but worth eliminating).
const daemonChildEnv = "DIR2MCP_DAEMON_CHILD"

// minDaemonChildNonceLen is the floor we require on the daemonChildEnv
// value before accepting it as proof that this process was spawned by a
// matching parent. Our parent sets a 32-character hex nonce; we accept
// 16+ chars to give us slack for future format changes while still
// rejecting "1" / "true" / other shell-exported values.
const minDaemonChildNonceLen = 16

// isRunningAsDaemonChild reports whether the current process was spawned
// by an earlier dir2mcp invocation as the daemon body. We require the
// env var to look like the parent-generated nonce rather than treating
// any presence as truthy, so a manually-exported `DIR2MCP_DAEMON_CHILD=1`
// can't accidentally trip the daemon code paths (which would silently
// drop the banner, the stdin quit listener, and the pid-file write —
// confusing footgun, no security boundary crossed).
func isRunningAsDaemonChild() bool {
	v := os.Getenv(daemonChildEnv)
	return len(v) >= minDaemonChildNonceLen
}

// pidFileName is the canonical filename for the dir2mcp daemon pid file
// inside the state directory. Kept short so it lines up with the other
// state files (connection.json, secret.token).
const pidFileName = "server.pid"

// serverLogName is the canonical filename for the daemon's redirected
// stdout/stderr inside the state directory. Append mode; never truncated.
const serverLogName = "server.log"

// daemonReadinessTimeout caps how long the parent will wait for the child
// to write connection.json before giving up and reporting bind failure.
// The server typically binds within ~1s; 15s is generous headroom for
// loaded developer machines.
const daemonReadinessTimeout = 15 * time.Second

// daemonReadinessPoll is the interval between connection.json existence
// checks during startup.
const daemonReadinessPoll = 150 * time.Millisecond

// daemonShutdownPoll is the interval `down` uses to poll process liveness
// after sending SIGTERM.
const daemonShutdownPoll = 200 * time.Millisecond

// daemonShutdownGrace is the maximum time `down` waits for a SIGTERM-ed
// process to exit before escalating to SIGKILL.
const daemonShutdownGrace = 5 * time.Second

// serverLogRotateBytes is the size threshold at which the parent rotates
// the existing server.log to server.log.1 when opening a new daemon.
// Single previous file is enough for "what happened on the last run"
// without committing to a real rotation library or risking unbounded
// disk use on a long-running daemon. 10 MB picked as the cheapest size
// that still captures a typical day of NDJSON traffic.
const serverLogRotateBytes int64 = 10 * 1024 * 1024

// pidFilePath returns the canonical pid file location for a given state dir.
func pidFilePath(stateDir string) string {
	return filepath.Join(stateDir, pidFileName)
}

// serverLogPath returns the canonical daemon log file location.
func serverLogPath(stateDir string) string {
	return filepath.Join(stateDir, serverLogName)
}

// connectionFilePath returns the canonical connection.json location.
func connectionFilePath(stateDir string) string {
	return filepath.Join(stateDir, connectionFileName)
}

// rotateLogIfLarge renames an oversized log file to "<path>.1" so the
// next daemon start has a fresh file to append to. Idempotent; missing
// file or undersized file are no-ops. The single rolled-over file
// preserves "what happened on the previous run" without a real rotation
// dependency.
func rotateLogIfLarge(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat log file: %w", err)
	}
	if info.Size() < serverLogRotateBytes {
		return nil
	}
	rolled := path + ".1"
	if err := os.Rename(path, rolled); err != nil {
		return fmt.Errorf("rotate log file: %w", err)
	}
	return nil
}

// claimPIDFile creates the pid file at path with O_EXCL so that two
// daemons racing to start in the same state directory cannot both
// succeed — only the kernel-level "create-if-not-exists" winner ends up
// owning the file. Returns an error wrapping os.ErrExist when another
// process has already claimed it.
//
// The child (not the parent) calls this once it has bound its listener.
// Doing the claim in the child rather than the parent fixes two
// reliability bugs raised in PR review:
//
//   - Parent crash between cmd.Start() and pid-file write would leak the
//     child if the parent owned the file. With the child as the writer,
//     the child registers itself and `dir2mcp down` can find it later
//     even when the parent never returned.
//   - Two `dir2mcp up` invocations racing in the same state dir both
//     pass the parent's already-running check and both spawn children;
//     with O_EXCL on the file, the second child loses deterministically
//     and exits without touching the first one's listener or files.
func claimPIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create pid file directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err // pass through os.ErrExist for callers to type-check
	}
	if _, err := fmt.Fprintf(f, "%d\n", pid); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write pid file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close pid file: %w", err)
	}
	return nil
}

// readPIDFile parses the pid from a pid file. Returns os.ErrNotExist when
// the file is absent so callers can treat "no daemon registered" as a
// distinct case from "pid file is malformed".
func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, fmt.Errorf("pid file %s is empty", path)
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("pid file %s contains non-integer %q", path, s)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pid file %s contains non-positive pid %d", path, pid)
	}
	return pid, nil
}

// removePIDFile deletes the pid file. Idempotent: a missing file is not
// an error.
func removePIDFile(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// processIsAlive reports whether a process with the given pid is currently
// alive and signalable by the calling user. It uses POSIX signal 0, which
// performs the kernel's permission/existence check without delivering a
// signal; ESRCH means "no such process", EPERM means "exists but not
// owned by us" (which we treat as "alive enough to leave alone").
func processIsAlive(pid int) bool {
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
	// Treat EPERM (process exists, signal blocked) as alive so we don't
	// blow away a pid file for a process we shouldn't touch.
	return errors.Is(err, syscall.EPERM)
}

// waitForConnectionFile blocks until the daemon child has written a fully
// populated connection.json (URL is set), the timeout expires, or the
// child process is observed to have exited (pid no longer alive). Returns
// the parsed payload.
//
// The childPid argument lets us surface a precise "the daemon exited
// before becoming ready" error instead of a generic timeout when the
// child died during startup — much more actionable for the user.
func waitForConnectionFile(path string, childPid int, timeout time.Duration) (connectionPayload, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil && len(raw) > 0:
			var p connectionPayload
			if jerr := json.Unmarshal(raw, &p); jerr == nil && strings.TrimSpace(p.URL) != "" {
				return p, nil
			} else if jerr != nil {
				lastErr = fmt.Errorf("parse %s: %w", path, jerr)
			}
		case err != nil && !errors.Is(err, os.ErrNotExist):
			lastErr = fmt.Errorf("read %s: %w", path, err)
		default:
			lastErr = fmt.Errorf("connection file %s not yet present", path)
		}
		if !processIsAlive(childPid) {
			return connectionPayload{}, fmt.Errorf("daemon (pid %d) exited before becoming ready", childPid)
		}
		if time.Now().After(deadline) {
			return connectionPayload{}, fmt.Errorf("timed out after %s waiting for daemon readiness: %v", timeout, lastErr)
		}
		time.Sleep(daemonReadinessPoll)
	}
}

// stopDaemon sends SIGTERM to pid, polls every daemonShutdownPoll for the
// process to exit, escalates to SIGKILL after daemonShutdownGrace, and
// returns nil when the process is no longer alive. Returns an error only
// when even SIGKILL leaves the process alive (which only happens when
// the process is owned by another user or is in an uninterruptible kernel
// state — both extreme).
func stopDaemon(pid int) error {
	if !processIsAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
	}
	deadline := time.Now().Add(daemonShutdownGrace)
	for time.Now().Before(deadline) {
		if !processIsAlive(pid) {
			return nil
		}
		time.Sleep(daemonShutdownPoll)
	}
	// Escalate.
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("send SIGKILL to %d: %w", pid, err)
	}
	// One more brief grace window for the kernel to reap.
	for i := 0; i < 5; i++ {
		if !processIsAlive(pid) {
			return nil
		}
		time.Sleep(daemonShutdownPoll)
	}
	return fmt.Errorf("process %d still alive after SIGKILL", pid)
}
