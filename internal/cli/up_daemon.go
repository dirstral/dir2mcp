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

	"github.com/dirstral/dir2mcp/internal/config"
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
func (a *App) runUpAsDaemonParent(ctx context.Context, opts upOptions) int {
	cfg, err := loadConfigForDaemonParent(opts.globalOptions)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	// Re-run the fast, config-only preconditions the child would run, so
	// a misconfig (missing MISTRAL_API_KEY, --public without auth) fails
	// here instead of after the full readiness timeout the user has to
	// debug from server.log. Listener-bind failures stay in the child — those
	// genuinely need the OS to refuse the port — and the readiness-error
	// path already points the user at the log for those.
	if code := a.preflightDaemonParentConfig(&cfg, opts); code != exitSuccess {
		return code
	}
	stateDir := cfg.StateDir
	pidPath := pidFilePath(stateDir)
	if code := a.prepareDaemonStateDir(stateDir, pidPath, opts); code != exitSuccess {
		return code
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

	// Pass the original argv through verbatim. The child re-enters runUp; the
	// daemon marker (set on Env below) takes the in-process branch instead of
	// recursing into the daemon parent.
	//
	// The marker is one half of a launch handshake: the nonce travels in the
	// environment, and the same nonce sits in an owner-only file the child
	// consumes (#671). Only the process this parent spawned can match it, so an
	// exported or inherited environment value cannot claim the role. The
	// cleanup removes the file when this parent returns, which bounds the
	// nonce's life to this launch even if the child died before it read it.
	childEnv, cleanupHandshake, err := prepareDaemonChildHandshake(stateDir)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("prepare daemon handshake: %v", err))
		return exitGeneric
	}
	defer cleanupHandshake()
	cmd := exec.Command(selfPath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv...)
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
		// Distinguish "still starting" (child alive, just slow — e.g. a cold
		// dir2mcp-full first run warming up the docling/torch probe) from a
		// real crash. The former is not a failure: the child keeps running
		// and will bind shortly, so report it as a friendly success instead
		// of the scary bind-failure path.
		if IsDaemonStillStarting(err) {
			// Deliberately no §7.7 coverage here, and the README says so. This
			// path returns while the child is still building the store, so any
			// verdict would describe a corpus that does not exist yet — and
			// "no uncovered formats" read off a half-built record is the kind
			// of false clean bill §7.7 exists to prevent. The operator is
			// pointed at the log, and the next `up` or `doctor` reports for
			// real.
			a.reportDaemonStillStarting(childPid, logPath, opts)
			return exitSuccess
		}
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

	a.printDaemonReady(ctx, cfg, logPath, registeredPid, connection, opts)
	return exitSuccess
}

// printDaemonReady renders the short connection summary the user sees
// when `dir2mcp up` returns control. Most of the ornate banner the in-process
// body would have produced lives in the server log; this is the just-enough
// info to keep working from the shell.
//
// The §7.7 coverage sections are the exception, and they are printed HERE rather
// than left to the log. SPEC §7.7 makes them startup diagnostics ("Startup
// diagnostics and `dir2mcp doctor` MUST report..."), and `up` daemonizes by
// default on a terminal, so before this they rendered nowhere on the path almost
// every operator takes: the child skips the banner entirely and never computes
// them, so they were not in the log either (#981).
func (a *App) printDaemonReady(ctx context.Context, cfg config.Config, logPath string, pid int, connection connectionPayload, opts upOptions) {
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
	a.printDaemonCoverage(ctx, s, cfg, opts)
	_, requiresAuth := connection.Headers["Authorization"]
	printRegistrationHint(a.stdout, s, cfg.ServerName, connection.URL, cfg.ProtocolVersion, requiresAuth)
	writef(a.stdout, "  %s\n", s.Success.Render("Ready for connections"))
	writef(a.stdout, "  %s\n\n", s.dim("Stop with: dir2mcp down"))
}

// reportDaemonStillStarting prints the friendly "still starting" notice for a
// healthy-but-slow daemon whose child is alive but hasn't bound within the
// readiness window (e.g. a cold dir2mcp-full first run warming up the
// docling/torch probe). This is NOT a failure: the child keeps running and
// will bind shortly, so the caller returns exitSuccess.
//
// In --json mode it emits a single clean, parseable object (status:
// "starting") on stdout rather than an error payload, so a still-starting
// result on a scripted run is a clear signal instead of a fatal error.
func (a *App) reportDaemonStillStarting(pid int, logPath string, opts upOptions) {
	if opts.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"status": "starting",
			"pid":    pid,
			"message": fmt.Sprintf(
				"daemon (pid %d) is still starting; first run can take longer "+
					"while models and the document extractor warm up", pid),
			"log":  logPath,
			"hint": "Check readiness with: dir2mcp status",
		})
		return
	}
	if opts.quiet {
		return
	}
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s %s\n", s.banner(), s.dim(fmt.Sprintf("daemon (pid %d) is still starting", pid)))
	writef(a.stdout, "  %s\n", s.dim("First run can take longer while models and the document extractor warm up."))
	writeln(a.stdout, s.kv("Logs", logPath))
	writef(a.stdout, "  %s\n\n", s.dim("Check readiness with: dir2mcp status"))
}

// prepareDaemonStateDir ensures the state directory exists, refuses to
// continue when an existing pid file points at a live process, and
// clears stale state from a previous run that would confuse the parent's
// readiness poll. Pulled out of runUpAsDaemonParent purely to keep that
// function under the gocyclo=15 budget after the preflight check landed.
//
// Stale-state cleanup notes:
//   - A pid file whose owner is dead is silently removed so the child's
//     O_EXCL claim has a clear field.
//   - A leftover connection.json from a previous run is removed because
//     waitForConnectionFile would otherwise observe the old file the
//     instant we start polling and return success before the new child
//     has bound — producing a confusing "daemon ready but pid file
//     missing" race window.
func (a *App) prepareDaemonStateDir(stateDir, pidPath string, opts upOptions) int {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("create state dir %s: %v", stateDir, err))
		return exitGeneric
	}
	// Only refuse when a live daemon that is actually OURS owns the pid file.
	// A recycled pid (alive, but its start-time token no longer matches — the
	// OS reassigned it after a crash without cleanup) must not be mistaken for
	// the daemon and block startup (issue #418); we clear that stale file and
	// proceed, same as a dead pid.
	if existing, ownership := classifyPIDFile(pidPath); ownership == pidLive {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric,
			fmt.Sprintf("dir2mcp is already running for %s (pid %d)", stateDir, existing),
			"Stop it with `dir2mcp down`, or pass --foreground to run a one-off in this terminal.",
		)
		return exitGeneric
	} else if ownership != pidNoFile {
		_ = removePIDFile(pidPath)
	}
	connPath := connectionFilePath(stateDir)
	if err := os.Remove(connPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "warning: clean stale %s: %v\n", connPath, err)
	}
	return exitSuccess
}

// preflightDaemonParentConfig runs the subset of config validation that
// can fail in the child for purely config-level reasons — i.e. would
// produce the same error regardless of whether the listener binds. By
// running these in the parent before fork(), we trade a full readiness
// timeout (and a misleading "did not become ready" message that frames
// a config bug as a daemon/network bug) for the immediate, accurate
// error the in-process body would have printed.
//
// Limited intentionally to the cfg-only checks: --public-requires-auth
// and missing MISTRAL_API_KEY. Port-bind failures and other runtime
// errors stay in the child — those need the OS state we can only
// observe after the listener attempts to bind, and the existing
// readiness-timeout path already directs the user to server.log for
// them (see TestDaemonLifecycle_BindBusyReportsClearError).
//
// Mutates cfg with the flag overrides the checks read (--listen,
// --auth, --public). That's safe because the spawned child re-loads
// config from scratch via its own loadConfigWithGlobalOptions call;
// the parent's mutations exist only for the duration of this preflight.
func (a *App) preflightDaemonParentConfig(cfg *config.Config, opts upOptions) int {
	applyScalarOverrides(cfg, opts)
	if cfg.Public || opts.public {
		if code := a.applyPublicMode(cfg, opts); code != exitSuccess {
			return code
		}
	}
	return a.checkMistralAPIKey(cfg, opts, upNonInteractiveMode(opts))
}

// generateDaemonNonce returns a 32-character hex string drawn from the system
// CSPRNG. It is the secret half of the daemon launch handshake, so the child
// can tell a parent-spawned launch from any other environment value (#671).
func generateDaemonNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read CSPRNG: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// printDaemonCoverage computes and prints the §7.7 coverage verdicts for the
// daemon parent, in the same order and with the same renderers the foreground
// banner uses, so the two paths cannot say different things about one corpus.
//
// The PARENT does this and the child still does not. Exactly one process should
// spend the query, and the parent is the one holding a terminal: the child's
// output goes to server.log, where a coverage warning reads as a server fault
// rather than as the diagnostic it is (#949).
//
// It runs after readiness, so the child has created and migrated the store. The
// read is concurrent with a running daemon, which is what `doctor` already does
// against a live server; both are readers, and the store is WAL.
//
// A failure is reported on stderr and swallowed otherwise. `up` has already
// succeeded by this point, and a corpus whose coverage could not be read is not
// a reason to fail a daemon that is serving — but it is a reason to say so,
// because a coverage report that fails quietly is the silence §7.7 forbids.
func (a *App) printDaemonCoverage(ctx context.Context, s styles, cfg config.Config, opts upOptions) {
	if opts.quiet || opts.jsonOutput {
		return
	}
	st := a.storeForConfig(cfg)
	if st == nil {
		return
	}
	defer func() { _ = st.Close() }()

	printCoverageSection(a.stdout, s, a.startupExtractionCoverage(ctx, st, cfg, opts, a.stderr))
	printTranscriptCoverageSection(a.stdout, s, a.startupTranscriptCoverage(ctx, st, cfg, opts, a.stderr))
}

// RenderDaemonReadyForTest renders the daemon PARENT's ready banner for cfg to
// the app's stdout, for the external tests package.
//
// It drives printDaemonReady end to end rather than printDaemonCoverage alone,
// because the defect in #981 was not that the sections rendered wrongly: it was
// that nothing on the daemon path ever asked for them. A test over the renderer
// in isolation would keep passing with the call deleted again.
func (a *App) RenderDaemonReadyForTest(ctx context.Context, cfg config.Config) {
	a.printDaemonReady(ctx, cfg, cfg.StateDir+"/server.log", 4242,
		connectionPayload{URL: "http://127.0.0.1:8765/mcp"}, upOptions{})
}

// ReportDaemonStillStartingForTest exposes the still-starting notice to the
// external tests package, so the §7.7 pairing can assert that this path emits
// no coverage rather than leaving that to a comment.
func (a *App) ReportDaemonStillStartingForTest(pid int, logPath string) {
	a.reportDaemonStillStarting(pid, logPath, upOptions{})
}
