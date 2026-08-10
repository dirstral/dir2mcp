package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
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

// daemonReadinessHeadroom is the slack added on top of the document-extractor
// probe ceiling to cover index load and the listener bind that follow the
// probe before the child writes connection.json. 25s on top of the 90s probe
// gives a 115s total readiness window.
const daemonReadinessHeadroom = 25 * time.Second

// daemonReadinessTimeout caps how long the parent will wait for the child
// to write connection.json before reporting the daemon as still-starting.
//
// The server itself binds within ~1s, but the child resolves the document
// extractor during startup BEFORE it binds. On dir2mcp-full with the default
// IngestExtractor="auto" and docling on PATH, that runs the docling
// functional probe (`docling --version`, which imports torch/transformers/cv2)
// bounded by ingest.doclingProbeTimeout (90s) on a cold first run. The readiness
// window MUST therefore stay comfortably above that probe ceiling, otherwise a
// healthy-but-slow first run trips a false "not ready" while the child is
// still warming up.
//
// We derive it from the probe ceiling (+ headroom) rather than hard-coding a
// figure so the ordering invariant — readiness window > probe ceiling — can't
// silently regress if the probe timeout is ever raised. Evaluates to 115s with
// today's 90s probe. Do NOT lower the probe timeout to "fix" a slow start.
var daemonReadinessTimeout = ingest.DoclingProbeTimeout() + daemonReadinessHeadroom

// DaemonReadinessTimeout exposes the readiness window so external tests can
// assert it stays above the document-extractor probe ceiling
// (ingest.DoclingProbeTimeout). Read-only accessor.
func DaemonReadinessTimeout() time.Duration { return daemonReadinessTimeout }

// errDaemonStillStarting is the sentinel waitForConnectionFile wraps when the
// readiness deadline expires while the child process is still alive. It marks
// the "healthy-but-slow" case (e.g. a cold dir2mcp-full first run warming up
// the docling/torch probe) so callers can report a friendly "still starting"
// message and succeed, rather than treating it as the hard bind failure used
// for a child that actually crashed. Use IsDaemonStillStarting to test for it.
var errDaemonStillStarting = errors.New("daemon still starting")

// IsDaemonStillStarting reports whether err indicates the daemon child was
// still alive but had not yet written connection.json when the readiness
// deadline expired (as opposed to having crashed). Exported so the readiness
// classification — still-starting vs crashed vs ready — can be unit-tested
// from the external tests package without exposing waitForConnectionFile.
func IsDaemonStillStarting(err error) bool {
	return errors.Is(err, errDaemonStillStarting)
}

// daemonReadinessPoll is the interval between connection.json existence
// checks during startup.
const daemonReadinessPoll = 150 * time.Millisecond

// daemonShutdownPoll is the interval `down` uses to poll process liveness
// after sending SIGTERM.
const daemonShutdownPoll = 200 * time.Millisecond

// daemonShutdownGrace is the maximum time `down` waits for a SIGTERM-ed
// process to exit before escalating to SIGKILL.
//
// It is derived from serverShutdownBudget, the worst-case wall time of a
// graceful stop (issue #688). The two must not drift apart. A grace period
// shorter than the budget makes the daemon's own drain theatre: `down` would
// SIGKILL a server that is still finishing an MCP request or writing its final
// index snapshot. The extra second covers process teardown after runUp returns.
const daemonShutdownGrace = serverShutdownBudget + time.Second

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

// teeServerLog mirrors the process-global logger to <state_dir>/server.log so
// recovered-panic stacks and log.Printf output reach the support bundle in
// EVERY launch mode — not just the double-fork daemon, whose stdout/stderr the
// parent already redirects to that file (issue #360). It is a no-op in the
// daemon child (its stderr is already the file, so teeing would double-write)
// and returns a restore func (or nil) so the global-logger mutation is bounded
// to the server's lifetime — important so it never leaks across tests. The tee
// is additive: existing destinations (terminal in the foreground, the service
// manager under launchd/systemd) keep receiving logs.
func (a *App) teeServerLog(stateDir string) func() {
	if isRunningAsDaemonChild() {
		return nil
	}
	logPath := serverLogPath(stateDir)
	if err := rotateLogIfLarge(logPath); err != nil {
		// Non-fatal: a failed rotation just means we keep appending.
		writef(a.stderr, "warning: rotate %s: %v\n", logPath, err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// Best-effort: if we can't open the log file, leave logging as-is
		// rather than failing server startup.
		writef(a.stderr, "warning: open server log %s: %v\n", logPath, err)
		return nil
	}
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, f))
	return func() {
		log.SetOutput(prev)
		_ = f.Close()
	}
}

// connectionFilePath returns the canonical connection.json location.
func connectionFilePath(stateDir string) string {
	return filepath.Join(stateDir, connectionFileName)
}

// readPreviousListenPort returns the TCP port recorded in a prior run's
// connection.json, or "" when none is recorded or it is unreadable. Used to make
// the ephemeral listen port sticky across restarts (#368).
func readPreviousListenPort(stateDir string) string {
	raw, err := os.ReadFile(connectionFilePath(stateDir))
	if err != nil {
		return ""
	}
	var conn connectionPayload
	if err := json.Unmarshal(raw, &conn); err != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(conn.URL))
	if err != nil {
		return ""
	}
	return u.Port()
}

// preferredListenAddr returns listenAddr unchanged unless it is the ephemeral
// default (host:0), in which case it substitutes the port a previous run
// recorded in connection.json so a restart/upgrade re-binds the SAME port and
// the URL baked into the Claude client config keeps working (#368). Returns the
// original ephemeral address when no prior port is recorded; callers should
// retry with the original address if binding the sticky port fails (the port may
// now be taken).
func preferredListenAddr(listenAddr, stateDir string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil || port != "0" {
		// Explicit port (operator pinned it) or unparseable: respect as-is.
		return listenAddr
	}
	// Prefer the port a previous run recorded in connection.json (#368).
	if prev := readPreviousListenPort(stateDir); prev != "" && prev != "0" {
		return net.JoinHostPort(host, prev)
	}
	// No recorded port — e.g. a fresh corpus, or after `down`/`rm -rf .dir2mcp`
	// (which the setup guide runs) removed connection.json. Derive a port that is
	// deterministic for this corpus instead of letting :0 pick a fresh random one
	// every time, so the URL baked into the client config stays valid across
	// reinstalls and the bridge does not silently strand (#386).
	if dp := deterministicPort(stateDir); dp != "" {
		return net.JoinHostPort(host, dp)
	}
	return listenAddr
}

// deterministicPort maps a corpus's state directory to a stable port in the IANA
// dynamic/private range (49152–65535) via a hash of its absolute path. The same
// corpus binds the same port across restarts even with no prior connection.json;
// distinct corpora almost always differ. bindServerListener still falls back to
// an ephemeral port if this one is taken, so startup never fails. Returns "" when
// the path can't be resolved (caller then keeps the ephemeral :0).
func deterministicPort(stateDir string) string {
	abs, err := filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil || abs == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(abs))
	const base, span = 49152, 16384 // 49152..65535 inclusive
	return strconv.Itoa(base + int(binary.BigEndian.Uint16(sum[:2]))%span)
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

// pidRecord is the parsed content of a daemon pid file: the registered pid
// plus, when the writing binary recorded it, an opaque start-time token that
// pins the pid to a specific process instance. StartToken is empty for a pid
// file written by a pre-#418 binary (a bare integer) or on a platform where
// processStartToken is unavailable; callers then fall back to bare liveness.
type pidRecord struct {
	PID        int
	StartToken string
}

// pidOwnership classifies a daemon pid file relative to the calling host so
// down/up/reindex/status can act correctly in the presence of pid reuse
// (issue #418): after a crash without cleanup the OS can recycle the recorded
// pid to an unrelated process, which a bare liveness check would mistake for
// the live daemon.
type pidOwnership int

const (
	pidNoFile    pidOwnership = iota // no pid file present
	pidMalformed                     // pid file unreadable / not a valid pid
	pidDead                          // names a process that is not alive (stale)
	pidRecycled                      // names a LIVE process that is not our daemon
	pidLive                          // names our live daemon (verified, or unverifiable)
)

// claimPIDFile creates the pid file at path with O_EXCL so that two
// daemons racing to start in the same state directory cannot both
// succeed — only the kernel-level "create-if-not-exists" winner ends up
// owning the file. Returns an error wrapping os.ErrExist when another
// process has already claimed it. The record includes this process's
// start-time token (when available) so a later down/up can tell our live
// daemon apart from an unrelated process that inherited a recycled pid.
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
	token, _ := processStartToken(pid)
	return writePIDRecord(path, pid, token, true)
}

// writePIDRecord writes the pid (and, when non-empty, its start-time token)
// to path. When exclusive is true it uses O_EXCL so a second racing writer
// fails with os.ErrExist (the daemon-claim path); otherwise it truncates,
// which is only used by the exported test helper.
func writePIDRecord(path string, pid int, startToken string, exclusive bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create pid file directory: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if exclusive {
		flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err // pass through os.ErrExist for callers to type-check
	}
	var buf strings.Builder
	_, _ = fmt.Fprintf(&buf, "%d\n", pid)
	if startToken != "" {
		_, _ = fmt.Fprintf(&buf, "start=%s\n", startToken)
	}
	if _, err := f.WriteString(buf.String()); err != nil {
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
// distinct case from "pid file is malformed". Extra lines (the start-time
// token) are ignored so callers that only need the pid stay simple.
func readPIDFile(path string) (int, error) {
	rec, err := readPIDRecord(path)
	if err != nil {
		return 0, err
	}
	return rec.PID, nil
}

// readPIDRecord parses the pid file into a pidRecord. The first line is the
// pid (as written by every binary, past and present); an optional
// `start=<token>` line pins that pid to a specific process instance. Returns
// os.ErrNotExist when the file is absent so callers can distinguish "no
// daemon registered" from a malformed file.
func readPIDRecord(path string) (pidRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pidRecord{}, err
	}
	lines := strings.Split(string(raw), "\n")
	first := strings.TrimSpace(lines[0])
	if first == "" {
		return pidRecord{}, fmt.Errorf("pid file %s is empty", path)
	}
	pid, err := strconv.Atoi(first)
	if err != nil {
		return pidRecord{}, fmt.Errorf("pid file %s contains non-integer %q", path, first)
	}
	if pid <= 0 {
		return pidRecord{}, fmt.Errorf("pid file %s contains non-positive pid %d", path, pid)
	}
	rec := pidRecord{PID: pid}
	for _, ln := range lines[1:] {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "start="); ok {
			rec.StartToken = v
		}
	}
	return rec, nil
}

// classifyPIDFile reads the pid file at path and reports both the recorded
// pid and how it relates to the calling host, so down/up/reindex/status can
// avoid the pid-reuse hazards in issue #418. When the record carries a
// start-time token and the live process's current token disagrees, the pid
// was recycled to an unrelated process (pidRecycled) and must NOT be
// signalled or mistaken for the daemon. When no token is recorded or the
// platform can't read one, an alive pid degrades to pidLive — identical to
// the pre-#418 bare-liveness behaviour.
func classifyPIDFile(path string) (int, pidOwnership) {
	rec, err := readPIDRecord(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, pidNoFile
		}
		return 0, pidMalformed
	}
	if !processIsAlive(rec.PID) {
		return rec.PID, pidDead
	}
	if rec.StartToken != "" {
		if tok, ok := processStartToken(rec.PID); ok && tok != rec.StartToken {
			return rec.PID, pidRecycled
		}
	}
	return rec.PID, pidLive
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

// ProcessStartTokenForTest exposes processStartToken so external lifecycle
// tests can tell whether pid-reuse protection is active on this platform (ok)
// and obtain a real token to build a deliberately-mismatching pid record.
// Test-only surface; not used by production code.
func ProcessStartTokenForTest(pid int) (string, bool) { return processStartToken(pid) }

// WritePIDRecordForTest writes a pid file carrying an arbitrary start-time
// token so external tests can simulate a recycled pid (a live pid whose
// recorded token no longer matches the live process). Test-only surface.
func WritePIDRecordForTest(path string, pid int, startToken string) error {
	return writePIDRecord(path, pid, startToken, false)
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
			// The child was observed alive this iteration (the exited check
			// above returned), so this is the healthy-but-slow case: it just
			// hasn't bound and written connection.json within the window.
			// Wrap the sentinel so the parent can report "still starting"
			// rather than the hard bind-failure used for a crashed child.
			return connectionPayload{}, fmt.Errorf(
				"timed out after %s waiting for daemon readiness: %v: %w",
				timeout, lastErr, errDaemonStillStarting,
			)
		}
		time.Sleep(daemonReadinessPoll)
	}
}

// WaitForConnectionReady is a thin exported wrapper over waitForConnectionFile
// so the readiness classification — ready vs still-starting vs crashed — can be
// exercised from the external tests package (waitForConnectionFile itself stays
// unexported). It returns whether the connection became ready and the
// classifying error; pair it with IsDaemonStillStarting to distinguish the
// healthy-but-slow case from a child that exited before binding.
func WaitForConnectionReady(path string, childPid int, timeout time.Duration) (bool, error) {
	if _, err := waitForConnectionFile(path, childPid, timeout); err != nil {
		return false, err
	}
	return true, nil
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
