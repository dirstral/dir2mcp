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

// daemonChildEnv is set on the child process so it knows it was spawned by
// the parent in daemon mode and should suppress the foreground banner +
// install signal handlers instead of the stdin quit listener.
const daemonChildEnv = "DIR2MCP_DAEMON_CHILD"

// isRunningAsDaemonChild reports whether the current process was spawned
// by an earlier dir2mcp invocation as the daemon body (not the launching
// parent). Detected via the daemonChildEnv marker that the parent sets
// on exec.Cmd.Env.
func isRunningAsDaemonChild() bool {
	return os.Getenv(daemonChildEnv) == "1"
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

// writePIDFile writes pid to path atomically via temp+rename so a crash
// mid-write can never leave a partially-written file that readPIDFile would
// reject as "invalid pid".
func writePIDFile(path string, pid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, pidFileName+".*")
	if err != nil {
		return fmt.Errorf("create temp pid file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := fmt.Fprintf(tmp, "%d\n", pid); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp pid file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp pid file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp pid file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename pid file: %w", err)
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
