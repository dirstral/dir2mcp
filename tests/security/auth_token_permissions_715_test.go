//go:build unix

// POSIX permission bits, syscall.Umask and syscall.Mkfifo. Windows has no
// umask and no mode bits: the owner-only guarantee there is an ACL contract
// (see statefs.HardenSecret), which is not what these tests measure. The
// non-regular-file refusal does apply on every platform.

package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

// #715: auto auth REUSED an existing <state-dir>/secret.token without looking
// at it. writeSecretToken passed 0600, but a create mode says nothing about a
// file that already exists, and readToken only read bytes. SPEC §4.2 requires
// that file to be owner-only.
//
// Measured on the unfixed tree, through a real `up`:
//
//   - a 0644 token was reused and ended up 0600, but only because #726's
//     HardenTree happens to walk the state directory earlier in startup, and
//     it does that silently. The auto-auth path itself enforced nothing.
//   - a secret.token SYMLINK survived untouched: HardenTree deliberately skips
//     links, so the token was read from outside the state directory at mode
//     0644 and the server started as if it were private.
//   - a symlink pointing at a file that trimmed to empty was written THROUGH:
//     the generated token truncated and overwrote that file, in place, at its
//     original 0644.
//
// The two symlink tests below fail on the unfixed tree (they exit 0 and the
// outside file is read/overwritten). The mode tests pin the policy: repair,
// never widen, never rotate behind the operator's back.

const seededToken = "seeded-token-that-clients-already-hold"

// upResult is what a scripted `up` run leaves behind for a test to measure.
type upResult struct {
	code   int
	stderr string
}

// runUpInDir runs `up` in dir with auto auth, returning as soon as the daemon
// has published connection.json (everything under test happens before that) or
// as soon as it exits on its own.
func runUpInDir(t *testing.T, dir string) upResult {
	t.Helper()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")
	// The startup embed probe is a live API call; it is unrelated to auth
	// material and would make this test depend on network reachability.
	t.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"}) }()

	connection := filepath.Join(dir, ".dir2mcp", "connection.json")
	deadline := time.Now().Add(25 * time.Second)
	for {
		select {
		case code := <-done:
			return upResult{code: code, stderr: stderr.String()}
		default:
		}
		if _, err := os.Stat(connection); err == nil {
			cancel()
			return upResult{code: <-done, stderr: stderr.String()}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("up neither started nor exited within the deadline: stderr=%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedCorpusWithToken lays out a corpus whose state directory was created by an
// older build (0755) and whose token carries mode.
func seedCorpusWithToken(t *testing.T, mode os.FileMode) (root, tokenPath string) {
	t.Helper()
	root = t.TempDir()
	state := filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("seed state dir: %v", err)
	}
	tokenPath = filepath.Join(state, "secret.token")
	if err := os.WriteFile(tokenPath, []byte(seededToken+"\n"), mode); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if err := os.Chmod(tokenPath, mode); err != nil { // defeat the umask on the seed
		t.Fatalf("seed token mode: %v", err)
	}
	if got := modeOf(t, tokenPath); got != mode {
		t.Fatalf("seed did not reproduce the reported state: token is %04o, want %04o", got, mode)
	}
	return root, tokenPath
}

func tokenOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// TestAutoAuthTightensAWorldReadableTokenAndKeepsIt: the repair-not-refuse and
// no-silent-rotation halves of the policy, end to end. Clients already hold
// this token; rotating it on a permission repair would break them without
// being asked.
func TestAutoAuthTightensAWorldReadableTokenAndKeepsIt(t *testing.T) {
	withPermissiveUmask(t)
	root, tokenPath := seedCorpusWithToken(t, 0o644)

	res := runUpInDir(t, root)
	if res.code != 0 {
		t.Fatalf("up refused a repairable token: exit=%d stderr=%s", res.code, res.stderr)
	}
	if got := modeOf(t, tokenPath); got != statefs.FileMode {
		t.Fatalf("reused token left at %04o, want %04o: another local account can read the bearer token", got, statefs.FileMode)
	}
	if got := tokenOf(t, tokenPath); got != seededToken {
		t.Fatalf("token was rotated behind the operator's back: got %q", got)
	}
}

// TestAutoAuthLeavesAPrivateTokenAlone: the common case stays silent and
// untouched: no warning, no rewrite.
func TestAutoAuthLeavesAPrivateTokenAlone(t *testing.T) {
	withPermissiveUmask(t)
	root, tokenPath := seedCorpusWithToken(t, statefs.FileMode)

	res := runUpInDir(t, root)
	if res.code != 0 {
		t.Fatalf("up failed on an already-private token: exit=%d stderr=%s", res.code, res.stderr)
	}
	if got := modeOf(t, tokenPath); got != statefs.FileMode {
		t.Fatalf("private token changed to %04o", got)
	}
	if got := tokenOf(t, tokenPath); got != seededToken {
		t.Fatalf("private token was rewritten: got %q", got)
	}
	if strings.Contains(res.stderr, "readable by other local accounts") {
		t.Fatalf("warned about an already-private token: stderr=%s", res.stderr)
	}
}

// TestAutoAuthDoesNotWidenAMoreRestrictiveToken: an operator who made the
// token read-only meant it. Enforcing 0600 must only ever remove bits.
func TestAutoAuthDoesNotWidenAMoreRestrictiveToken(t *testing.T) {
	withPermissiveUmask(t)
	root, tokenPath := seedCorpusWithToken(t, 0o400)

	res := runUpInDir(t, root)
	if res.code != 0 {
		t.Fatalf("up failed on a 0400 token: exit=%d stderr=%s", res.code, res.stderr)
	}
	if got := modeOf(t, tokenPath); got != 0o400 {
		t.Fatalf("a deliberately restrictive token was widened to %04o", got)
	}
	if got := tokenOf(t, tokenPath); got != seededToken {
		t.Fatalf("0400 token was rewritten: got %q", got)
	}
}

// TestAutoAuthCreatesAFreshTokenOwnerOnly: the create path, under the umask
// that made this defect class invisible.
func TestAutoAuthCreatesAFreshTokenOwnerOnly(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()

	res := runUpInDir(t, root)
	if res.code != 0 {
		t.Fatalf("up failed on a fresh corpus: exit=%d stderr=%s", res.code, res.stderr)
	}
	tokenPath := filepath.Join(root, ".dir2mcp", "secret.token")
	if got := modeOf(t, tokenPath); got != statefs.FileMode {
		t.Fatalf("freshly created token is %04o, want %04o", got, statefs.FileMode)
	}
	if got := tokenOf(t, tokenPath); len(got) != 64 {
		t.Fatalf("unexpected generated token length %d", len(got))
	}
}

// TestAutoAuthRefusesASymlinkedToken: HardenTree skips symlinks, so this is
// the case #726 could not reach. On the unfixed tree the server started and
// used a 0644 token living outside the state directory.
func TestAutoAuthRefusesASymlinkedToken(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".dir2mcp"), 0o755); err != nil {
		t.Fatalf("seed state dir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.token")
	if err := os.WriteFile(outside, []byte("token-someone-else-can-read\n"), 0o644); err != nil {
		t.Fatalf("seed outside token: %v", err)
	}
	if err := os.Chmod(outside, 0o644); err != nil {
		t.Fatalf("seed outside mode: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".dir2mcp", "secret.token")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := runUpInDir(t, root)
	if res.code == 0 {
		t.Fatalf("up served a symlinked secret.token as if it were private: stderr=%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "--auth file:") {
		t.Fatalf("refusal does not name the operator-managed escape hatch: stderr=%s", res.stderr)
	}
	if got := modeOf(t, outside); got != 0o644 {
		t.Fatalf("refusal reached through the link and chmod'd %s to %04o", outside, got)
	}
	if got := tokenOf(t, outside); got != "token-someone-else-can-read" {
		t.Fatalf("refusal rewrote the link target: got %q", got)
	}
}

// TestAutoAuthDoesNotWriteThroughASymlink: the sharper half. With an empty
// target the auto path GENERATES a token, and on the unfixed tree wrote it
// through the link, truncating a file outside the state directory.
func TestAutoAuthDoesNotWriteThroughASymlink(t *testing.T) {
	withPermissiveUmask(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".dir2mcp"), 0o755); err != nil {
		t.Fatalf("seed state dir: %v", err)
	}
	victim := filepath.Join(t.TempDir(), "victim.conf")
	const victimContent = "   \n" // non-empty file, empty token
	if err := os.WriteFile(victim, []byte(victimContent), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(root, ".dir2mcp", "secret.token")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := runUpInDir(t, root)
	if res.code == 0 {
		t.Fatalf("up accepted a symlinked secret.token: stderr=%s", res.stderr)
	}
	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(raw) != victimContent {
		t.Fatalf("a generated token was written through the symlink into %s: %q", victim, string(raw))
	}
}

// TestHardenSecretReportsThePriorMode: the primitive the CLI warning is built
// on. A caller cannot tell the operator what it repaired unless the repair
// reports what it found.
func TestHardenSecretReportsThePriorMode(t *testing.T) {
	withPermissiveUmask(t)
	dir := t.TempDir()

	for _, tc := range []struct {
		name  string
		seed  os.FileMode
		final os.FileMode
		wider bool
	}{
		{"world readable", 0o644, statefs.FileMode, true},
		{"group readable", 0o640, statefs.FileMode, true},
		{"already private", 0o600, 0o600, false},
		{"more restrictive", 0o400, 0o400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte("token\n"), tc.seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := os.Chmod(path, tc.seed); err != nil {
				t.Fatalf("seed mode: %v", err)
			}
			prior, exists, err := statefs.HardenSecret(path)
			if err != nil {
				t.Fatalf("HardenSecret: %v", err)
			}
			if !exists {
				t.Fatal("existing file reported as missing")
			}
			if prior != tc.seed {
				t.Fatalf("prior mode reported as %04o, want %04o", prior, tc.seed)
			}
			if got := statefs.WiderThanOwnerOnly(prior); got != tc.wider {
				t.Fatalf("WiderThanOwnerOnly(%04o)=%v, want %v", prior, got, tc.wider)
			}
			if got := modeOf(t, path); got != tc.final {
				t.Fatalf("file left at %04o, want %04o", got, tc.final)
			}
		})
	}
}

// TestHardenSecretOnAMissingPath: creating the token is the caller's job, so a
// missing path is a report, not an error.
func TestHardenSecretOnAMissingPath(t *testing.T) {
	withPermissiveUmask(t)
	prior, exists, err := statefs.HardenSecret(filepath.Join(t.TempDir(), "secret.token"))
	if err != nil {
		t.Fatalf("HardenSecret on a missing path: %v", err)
	}
	if exists {
		t.Fatal("missing path reported as existing")
	}
	if prior != 0 {
		t.Fatalf("missing path reported prior mode %04o", prior)
	}
}

// TestHardenSecretRefusesNonRegularPaths: a FIFO would hang the startup that
// read it; a directory and a symlink are not credentials either.
func TestHardenSecretRefusesNonRegularPaths(t *testing.T) {
	withPermissiveUmask(t)
	dir := t.TempDir()

	fifo := filepath.Join(dir, "fifo.token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	subdir := filepath.Join(dir, "dir.token")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("seed mode: %v", err)
	}
	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, path := range []string{fifo, subdir, link} {
		_, _, err := statefs.HardenSecret(path)
		if err == nil {
			t.Fatalf("HardenSecret accepted a non-regular path: %s", path)
		}
		if !errors.Is(err, statefs.ErrNotRegular) {
			t.Fatalf("unexpected error for %s: %v", path, err)
		}
	}
	if got := modeOf(t, target); got != 0o644 {
		t.Fatalf("HardenSecret chmod'd through a symlink: target is %04o", got)
	}
}
