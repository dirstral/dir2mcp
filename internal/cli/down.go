package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runDown stops the dir2mcp daemon registered for the current state
// directory. Idempotent: a missing or stale pid file is reported as
// "no server running" with exit 0 so teardown scripts can run
// unconditionally.
//
// The state directory is resolved the same way `dir2mcp up` resolves it,
// so users typically just `cd` to the directory they invoked `up` in and
// run `dir2mcp down`.
func (a *App) runDown(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid down flags: %v", err))
		return exitConfigInvalid
	}
	if fs.NArg() > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("down does not accept positional arguments: %s", strings.Join(fs.Args(), " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}

	pidPath := pidFilePath(cfg.StateDir)
	pid, ownership := classifyPIDFile(pidPath)
	switch ownership {
	case pidNoFile:
		writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, 0, false, "no_pid_file")
		return exitSuccess
	case pidMalformed:
		// Malformed pid file is suspicious — surface and continue cleanup so
		// `down` always leaves a clean state behind.
		writef(a.stderr, "warning: pid file %s is malformed; removing it\n", pidPath)
		_ = removePIDFile(pidPath)
		writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, 0, false, "malformed_pid_file")
		return exitSuccess
	case pidDead:
		_ = removePIDFile(pidPath)
		writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, pid, false, "stale_pid")
		return exitSuccess
	case pidRecycled:
		// The recorded pid is alive but its start-time token no longer matches:
		// the OS recycled it to an unrelated process after our daemon crashed
		// without cleanup. Signalling it would SIGTERM/SIGKILL a bystander
		// (issue #418) — so clean up the stale pid file and stop, killing
		// nothing.
		_ = removePIDFile(pidPath)
		writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, pid, false, "recycled_pid")
		return exitSuccess
	}

	if err := stopDaemon(pid); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("stop pid %d: %v", pid, err))
		return exitGeneric
	}
	if err := removePIDFile(pidPath); err != nil {
		// Log only — the process is gone, the pid file is residue. Don't fail
		// the user-facing exit code over a leftover file.
		writef(a.stderr, "warning: remove pid file %s: %v\n", pidPath, err)
	}
	// Also remove the connection.json the daemon wrote so a subsequent
	// `dir2mcp up` doesn't observe a stale "daemon ready" file from the
	// now-stopped server. See the same cleanup in runUpAsDaemonParent.
	connPath := connectionFilePath(cfg.StateDir)
	if err := os.Remove(connPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "warning: remove %s: %v\n", connPath, err)
	}
	writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, pid, true, "stopped")
	return exitSuccess
}

// writeDownInfo emits the user-facing result of `down`. JSON mode emits a
// structured object so scripts can branch on the outcome; humans get a
// short prose line.
func writeDownInfo(stdout io.Writer, jsonMode bool, stateDir string, pid int, stopped bool, reason string) {
	if jsonMode {
		_ = emitJSON(stdout, map[string]interface{}{
			"state_dir": stateDir,
			"pid":       pid,
			"stopped":   stopped,
			"reason":    reason,
		})
		return
	}
	switch reason {
	case "stopped":
		writef(stdout, "stopped dir2mcp daemon (pid %d) for %s\n", pid, stateDir)
	case "stale_pid":
		writef(stdout, "no dir2mcp daemon was running for %s (cleared stale pid %d)\n", stateDir, pid)
	case "recycled_pid":
		writef(stdout, "no dir2mcp daemon running for %s; pid %d was recycled to an unrelated process and left untouched (cleared stale pid file)\n", stateDir, pid)
	case "no_pid_file":
		writef(stdout, "no dir2mcp daemon registered for %s\n", stateDir)
	case "malformed_pid_file":
		writef(stdout, "removed malformed pid file in %s; nothing to stop\n", stateDir)
	default:
		writef(stdout, "no dir2mcp daemon to stop in %s (%s)\n", stateDir, reason)
	}
}
