package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
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
	if ownership != pidLive {
		return a.downWithoutLiveDaemon(global, cfg.StateDir, pidPath, pid, ownership)
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
	// The daemon is stopped, so its published endpoint is dead. Clear it only
	// here, after the stop succeeded: a failed stop returns above and keeps
	// connection.json, because a daemon that still serves must stay reachable.
	a.clearConnectionFile(cfg.StateDir)
	// If this corpus is also under launchd/systemd supervision, the daemon we
	// just stopped will be respawned at next login/boot. Inform the operator
	// (do NOT auto-uninstall) so `down` in a teardown script isn't mistaken
	// for a permanent stop (#434).
	writeDownInfo(a.stdout, global.jsonOutput, cfg.StateDir, pid, true, "stopped", a.corpusServiceManaged(cfg, global))
	return exitSuccess
}

// downWithoutLiveDaemon handles every pid classification that proves no owned
// live daemon exists: no pid file, a malformed record, a dead pid, or a pid the
// OS recycled to an unrelated process. Each case clears the pid record and the
// published connection.json, then reports the outcome with exit 0.
//
// The pid record goes first and the connection file second. If the command dies
// between the two steps, the next `down` classifies the corpus as pidNoFile and
// removes the connection file, so the state directory still converges to clean.
// A recycled pid is never signalled (#418): the process is a bystander.
func (a *App) downWithoutLiveDaemon(global globalOptions, stateDir, pidPath string, pid int, ownership pidOwnership) int {
	// The no-pid-file case reports pid 0, as does a malformed record: neither
	// names a process the operator can act on.
	reason, reportedPID := "no_pid_file", 0
	switch ownership {
	case pidMalformed:
		// A malformed pid file is suspicious. Report it, then clean up so
		// `down` always leaves a clean state behind.
		writef(a.stderr, "warning: pid file %s is malformed; removing it\n", pidPath)
		reason = "malformed_pid_file"
	case pidDead:
		reason, reportedPID = "stale_pid", pid
	case pidRecycled:
		reason, reportedPID = "recycled_pid", pid
	}
	if ownership != pidNoFile {
		_ = removePIDFile(pidPath)
	}
	// No daemon owns this state directory, so any connection.json is residue
	// from a crashed or already-stopped run. Clients and installers read that
	// file as the connection source, so a successful `down` must not leave a
	// dead endpoint behind (#714).
	a.clearConnectionFile(stateDir)
	writeDownInfo(a.stdout, global.jsonOutput, stateDir, reportedPID, false, reason, false)
	return exitSuccess
}

// clearConnectionFile removes the connection.json published for stateDir so a
// later `up`, client, or bridge does not read a stale endpoint. An absent file
// is a normal result and keeps `down` idempotent. A removal failure is a
// warning only: the daemon is already gone, so the leftover file must not
// change the exit code.
func (a *App) clearConnectionFile(stateDir string) {
	connPath := connectionFilePath(stateDir)
	if err := os.Remove(connPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "warning: remove %s: %v\n", connPath, err)
	}
}

// corpusServiceManaged reports whether a launchd/systemd unit is installed
// for the current corpus. Best-effort: any resolution/backend error (e.g. an
// unsupported platform) yields false so the informational note is simply
// omitted and `down` never fails over it. Takes the already-loaded config so it
// doesn't reload it.
func (a *App) corpusServiceManaged(cfg config.Config, global globalOptions) bool {
	sc, err := serviceContextFromConfig(cfg, "", resolveConfigPath(global))
	if err != nil {
		return false
	}
	mgr, err := newServiceManager()
	if err != nil {
		return false
	}
	state, err := mgr.status(sc.label)
	if err != nil {
		return false
	}
	return state.Installed
}

// writeDownInfo emits the user-facing result of `down`. JSON mode emits a
// structured object so scripts can branch on the outcome; humans get a
// short prose line.
func writeDownInfo(stdout io.Writer, jsonMode bool, stateDir string, pid int, stopped bool, reason string, serviceManaged bool) {
	if jsonMode {
		_ = emitJSON(stdout, map[string]interface{}{
			"state_dir":       stateDir,
			"pid":             pid,
			"stopped":         stopped,
			"reason":          reason,
			"service_managed": serviceManaged,
		})
		return
	}
	switch reason {
	case "stopped":
		writef(stdout, "stopped dir2mcp daemon (pid %d) for %s\n", pid, stateDir)
		if serviceManaged {
			writeln(stdout, "note: this corpus is service-managed; it will restart at next login/boot — run `dir2mcp service uninstall` to remove permanently")
		}
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
