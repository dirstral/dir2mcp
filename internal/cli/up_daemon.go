package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runUpAsDaemonParent spawns a detached child process that runs the
// in-process server body, then exits the parent once the child has
// written connection.json. The user's shell prompt returns immediately
// instead of being held by `up`'s event loop.
//
// Lifecycle:
//
//  1. Resolve the state directory the same way the in-process body
//     does — by calling loadConfigWithGlobalOptions — so we can find
//     (or create) the pid/log files in the same location the child
//     will use.
//  2. If a pid file already exists for an alive process, refuse with a
//     clear "already running" message rather than leaving the user
//     with two competing daemons.
//  3. Spawn os.Args[0] with the same argv plus the daemonChildEnv
//     marker, redirecting stdin/stdout/stderr away from the terminal
//     and calling setsid so terminal-close doesn't SIGHUP the child.
//  4. Wait (up to daemonReadinessTimeout) for the child to write a
//     populated connection.json — that's the canonical "server is
//     bound and accepting" signal.
//  5. Atomically write the pid file, print a short ready summary to
//     the user's terminal, and return exitSuccess. The child keeps
//     running.
func (a *App) runUpAsDaemonParent(_ context.Context, opts upOptions) int {
	cfg, err := loadConfigForDaemonParent(opts.globalOptions)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	stateDir := cfg.StateDir
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("create state dir %s: %v", stateDir, err))
		return exitGeneric
	}

	pidPath := pidFilePath(stateDir)
	if existing, err := readPIDFile(pidPath); err == nil {
		if processIsAlive(existing) {
			writeCLIError(a.stderr, opts.jsonOutput, exitGeneric,
				fmt.Sprintf("dir2mcp is already running for %s (pid %d)", stateDir, existing),
				"Stop it with `dir2mcp down`, or pass --foreground to run a one-off in this terminal.",
			)
			return exitGeneric
		}
		// Stale pid file — silently clean it up so the spawn below has a
		// clear field to write into when the child becomes ready.
		_ = removePIDFile(pidPath)
	}
	// Also remove a stale connection.json from a previous run. Without
	// this, the parent's waitForConnectionFile would observe the old
	// file the instant we start polling and return success before the
	// new child has bound or claimed the pid file — producing a
	// confusing "daemon ready but pid file missing" race window.
	connPath := connectionFilePath(stateDir)
	if err := os.Remove(connPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "warning: clean stale %s: %v\n", connPath, err)
	}

	logPath := serverLogPath(stateDir)
	if err := rotateLogIfLarge(logPath); err != nil {
		// Non-fatal: a failed rotation just means the new daemon keeps
		// appending to the existing log. Surface it so an unbounded
		// log can't sneak past us silently.
		writef(a.stderr, "warning: rotate %s: %v\n", logPath, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("open server log %s: %v", logPath, err))
		return exitGeneric
	}
	defer func() { _ = logFile.Close() }()

	selfPath, err := os.Executable()
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("locate dir2mcp executable: %v", err))
		return exitGeneric
	}

	// Pass the original argv through verbatim. The child re-enters
	// runUp; the daemonChildEnv marker (set on Env below) takes the
	// in-process branch instead of recursing into the daemon parent.
	// The marker value is a per-spawn random nonce rather than a
	// constant "1" so a manually-exported env var in the user's shell
	// can't accidentally trip the daemon-child code paths.
	nonce, err := generateDaemonNonce()
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("generate daemon nonce: %v", err))
		return exitGeneric
	}
	cmd := exec.Command(selfPath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), daemonChildEnv+"="+nonce)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachFromTerminal(cmd)

	if err := cmd.Start(); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("spawn dir2mcp daemon: %v", err))
		return exitGeneric
	}
	childPid := cmd.Process.Pid

	// Release the child's process handle so the parent doesn't keep a
	// pointer to a process it'll never wait on. Note: it's the Setsid
	// in detachFromTerminal (above) that actually allows the child to
	// outlive the parent — Setsid puts the child in a new session so
	// the parent's exit doesn't deliver SIGHUP. Process.Release just
	// cleans up the Go-side handle so we don't carry around a wait()
	// debt for a process we don't manage. The OS reparents the child
	// to init/launchd as a normal consequence of the parent exiting.
	if err := cmd.Process.Release(); err != nil {
		writef(a.stderr, "warning: release child process: %v\n", err)
	}

	connection, err := waitForConnectionFile(connectionFilePath(stateDir), childPid, daemonReadinessTimeout)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitServerBindFailure,
			fmt.Sprintf("daemon (pid %d) did not become ready: %v", childPid, err),
			fmt.Sprintf("Inspect %s for startup errors.", logPath),
		)
		return exitServerBindFailure
	}

	// Pid file is written by the child (claimPIDFile with O_EXCL) so
	// the file always reflects a process the OS confirmed existed at
	// the moment of registration, even if the parent crashes between
	// cmd.Start and here. Read it back to surface the actual pid the
	// child registered — useful when a concurrent `up` race causes our
	// child to lose the O_EXCL claim and the file points at the
	// winning sibling instead of cmd.Process.Pid.
	registeredPid, err := readPIDFile(pidPath)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric,
			fmt.Sprintf("daemon ready but pid file %s missing or unreadable: %v", pidPath, err),
			fmt.Sprintf("Inspect %s for startup errors.", logPath),
		)
		return exitGeneric
	}

	a.printDaemonReady(cfg.StateDir, logPath, registeredPid, connection, opts)
	return exitSuccess
}

// printDaemonReady renders the short connection summary the user sees
// when `dir2mcp up` returns control. The full ornate banner that the
// in-process body would have produced lives in the server log; this is
// the just-enough info to keep working from the shell.
func (a *App) printDaemonReady(stateDir, logPath string, pid int, connection connectionPayload, opts upOptions) {
	if opts.quiet {
		return
	}
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s %s %s\n", s.banner(), s.dim("daemon"), s.dim(fmt.Sprintf("pid %d", pid)))
	writeln(a.stdout, s.separator(44))
	writeln(a.stdout)
	writeln(a.stdout, s.kv("URL", s.URL.Render(connection.URL)))
	if connection.TokenFile != "" {
		writeln(a.stdout, s.kv("Token file", connection.TokenFile))
	}
	if v := strings.TrimSpace(connection.Headers["MCP-Protocol-Version"]); v != "" {
		writeln(a.stdout, s.kv("Protocol", v))
	}
	writeln(a.stdout, s.kv("Logs", logPath))
	writeln(a.stdout)
	writef(a.stdout, "  %s\n", s.Success.Render("Ready for connections"))
	writef(a.stdout, "  %s\n\n", s.dim("Stop with: dir2mcp down"))
}

// generateDaemonNonce returns a 32-character hex string drawn from the
// system CSPRNG. Used as the daemonChildEnv value so isRunningAsDaemonChild
// can distinguish a parent-spawned child from a manually-exported env var.
func generateDaemonNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read CSPRNG: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
