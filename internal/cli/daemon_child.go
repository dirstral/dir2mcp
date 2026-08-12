package cli

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// Daemon-child identity (issue #671).
//
// `dir2mcp up` daemonizes by spawning itself: the parent starts a detached
// copy of the binary, and that copy runs the server body. The copy must know
// which of the two roles it holds, because the daemon body drops the banner,
// drops the stdin quit listener, skips the server.log tee (its stderr is
// already that file) and installs the SIGTERM handler `dir2mcp down` relies
// on. The parent marks the copy through the environment.
//
// The marker used to be accepted on length alone: any value of 16 or more
// characters selected daemon-child behaviour. A length check is not an
// identity check. An exported shell variable, a variable inherited from a CI
// runner, or a value injected into the environment of a normal `dir2mcp up`
// therefore turned that run into a foreground-attached process that behaved
// like a daemon body: silent, unstoppable with q+Enter, and with its log tee
// disabled. A grandchild process inherited the marker and claimed the role
// too.
//
// The marker is now proof of one specific launch. The parent draws a 32
// hex-character CSPRNG nonce, writes it to an owner-only file inside the
// state directory, and passes both the nonce and the file path to the child.
// The child compares the value it was handed with the file contents in
// constant time and consumes the file, so:
//
//   - An arbitrary value never verifies, whatever its length.
//   - The nonce admits exactly one process, once.
//   - A grandchild that inherits the environment finds the file gone.
//
// Nothing here logs the marker, the nonce or the file contents. The value is
// a shared secret for the lifetime of one spawn.

// daemonChildEnv carries the per-spawn nonce the parent hands to the child.
const daemonChildEnv = "DIR2MCP_DAEMON_CHILD"

// daemonChildHandshakeEnv carries the path of the file that holds the same
// nonce. The child needs the path before it has loaded any config, so the
// parent passes it in the environment rather than deriving it.
const daemonChildHandshakeEnv = "DIR2MCP_DAEMON_HANDSHAKE"

// daemonChildNonceHexLen is the exact length of the hex nonce
// generateDaemonNonce produces (16 random bytes). The length is a published
// constant of the format, not a secret, so rejecting a wrong-length value
// before the constant-time compare reveals nothing.
const daemonChildNonceHexLen = 32

// daemonChildHandshakeMaxBytes bounds the read of the handshake file so a
// hostile or corrupt file cannot make startup allocate.
const daemonChildHandshakeMaxBytes = 128

// daemonChildHandshakeMode is the mode the handshake file must carry: readable
// and writable by the owner only. The child refuses a group- or world-readable
// file, because the nonce is a secret for the duration of the spawn.
const daemonChildHandshakeMode os.FileMode = 0o600

// isDaemonChild reports whether this process is the daemon body an earlier
// dir2mcp invocation spawned. It verifies the launch handshake once per
// process and caches the verdict, which matters for correctness: the parent
// removes the handshake file once the child reports ready, so a second,
// unverified evaluation later in the child's life would flip the role
// mid-flight and change how the running server handles signals and logs.
//
// A marker that is present but does not verify is reported to stderr once,
// because the run then behaves differently from what the operator who set the
// variable expected. The message names the variable and nothing else; the
// value is never printed.
func (a *App) isDaemonChild() bool {
	a.daemonChildOnce.Do(func() {
		a.daemonChildVerified = a.verifyDaemonChildHandshake()
		if !a.daemonChildVerified && os.Getenv(daemonChildEnv) != "" {
			writef(a.stderr, "warning: %s is set but the daemon launch handshake did not verify; running in the foreground\n", daemonChildEnv)
		}
	})
	return a.daemonChildVerified
}

// verifyDaemonChildHandshake compares the marker this process was handed with
// the nonce the spawning parent wrote to the handshake file, in constant time.
// It returns true only for an exact match.
//
// A successful match consumes the file. The nonce then admits no further
// process: a stale environment, a replayed value or an inherited grandchild
// environment all fail the next check. A failed match leaves the file alone,
// so a process that guesses wrong cannot deny the real child its handshake;
// the parent removes the file when it exits either way.
//
// A failed removal does not withdraw a verdict this process has already
// proved. The daemon body has no terminal, so a withdrawn verdict would start
// the stdin quit listener on /dev/null and stop the server on the EOF it reads
// at once. The residual risk is small in exchange: the file stays readable to
// the owner of the state directory only, and the parent removes it when it
// returns. The failure is reported so it is not silent.
func (a *App) verifyDaemonChildHandshake() bool {
	claim := os.Getenv(daemonChildEnv)
	path := strings.TrimSpace(os.Getenv(daemonChildHandshakeEnv))
	if len(claim) != daemonChildNonceHexLen || path == "" {
		return false
	}
	expected, ok := readDaemonChildHandshake(path)
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(claim), []byte(expected)) != 1 {
		return false
	}
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		writef(a.stderr, "warning: consume daemon handshake file: %v\n", rerr)
	}
	return true
}

// readDaemonChildHandshake returns the nonce recorded in the handshake file.
// It reports false when the file is missing, is not a regular file, is
// readable beyond its owner, or cannot be read. Errors are deliberately not
// reported: every one of them means "this process is not the daemon child",
// which the caller handles, and the file contents must never reach a log.
//
// The path arrives in the environment, so it is not trusted. The open is
// non-blocking: a plain open of a FIFO waits for a writer, which would hang
// `dir2mcp up` before it starts. The type check below then rejects every
// non-regular file the non-blocking open let through.
func readDaemonChildHandshake(path string) (string, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(f, daemonChildHandshakeMaxBytes))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

// prepareDaemonChildHandshake creates the single-use handshake for one spawn
// and returns the environment entries the child must receive, plus a cleanup
// that removes the handshake file.
//
// The caller must run the cleanup before it returns. The child normally
// consumes the file itself; the cleanup covers a child that died before it
// got that far, so a nonce never outlives the launch it belongs to.
func prepareDaemonChildHandshake(stateDir string) (env []string, cleanup func(), err error) {
	nonce, err := generateDaemonNonce()
	if err != nil {
		return nil, nil, err
	}
	// The file lives in the state directory: it is owner-only, it is on the
	// same volume the daemon already writes, and `rm -rf .dir2mcp` clears it.
	f, err := os.CreateTemp(stateDir, "daemon-handshake-*.nonce")
	if err != nil {
		return nil, nil, fmt.Errorf("create daemon handshake file: %w", err)
	}
	path := f.Name()
	cleanup = func() { _ = os.Remove(path) }
	// CreateTemp asks for 0600, but the process umask can strip owner bits from
	// that mode, and the child rejects a file it cannot read. Set the mode
	// explicitly so the handshake never depends on the umask of the shell.
	if cerr := f.Chmod(daemonChildHandshakeMode); cerr != nil {
		_ = f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("harden daemon handshake file: %w", cerr)
	}
	if _, werr := f.WriteString(nonce + "\n"); werr != nil {
		_ = f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("write daemon handshake file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("close daemon handshake file: %w", cerr)
	}
	return []string{
		daemonChildEnv + "=" + nonce,
		daemonChildHandshakeEnv + "=" + path,
	}, cleanup, nil
}

// DaemonChildHandshakeEnvForTest prepares a real parent-side handshake in
// stateDir and returns the environment entries a spawned child receives, plus
// a cleanup that removes the handshake file. It lets external tests reproduce
// the launch handshake exactly, rather than assert that a long-enough marker
// selects daemon-child mode. Test-only surface; not used by production code.
func DaemonChildHandshakeEnvForTest(stateDir string) ([]string, func(), error) {
	return prepareDaemonChildHandshake(stateDir)
}
