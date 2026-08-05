//go:build unix

// POSIX permission bits and syscall.Umask. Windows has neither, and its
// owner-only guarantee is an ACL contract tested separately.

package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

// #719: `support-bundle --output <path>` documents an owner-only 0600 archive,
// but it opened the destination with
//
//	os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
//
// and an open mode is applied only when the file is CREATED. Overwriting an
// existing group/world-readable bundle truncated it in place and left the mode
// alone, so a bundle carrying server.log and diagnostic metadata stayed
// readable by every local account. The promise held only for the first write to
// a given path.
//
// These tests force a permissive umask because the defect is invisible under a
// restrictive one: with umask 077 a fresh bundle is 0600 whatever the code
// does, and the overwrite case needs a permissive file to have existed at all.
// withPermissiveUmask and modeOf are shared with state_permissions_726_test.go.

// runSupportBundleTo writes a bundle to dest and fails the test if the command
// does not succeed.
func runSupportBundleTo(t *testing.T, stateDir, dest string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	if code := app.Run([]string{"--state-dir", stateDir, "support-bundle", "--output", dest}); code != 0 {
		t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
	}
}

// TestSupportBundleOverwritingAPermissiveDestinationTightensIt is the reported
// defect. It fails on the pre-fix code with mode 0644/0666 preserved.
func TestSupportBundleOverwritingAPermissiveDestinationTightensIt(t *testing.T) {
	withPermissiveUmask(t)

	for _, seedMode := range []os.FileMode{0o644, 0o666} {
		t.Run(seedMode.String(), func(t *testing.T) {
			tmp := t.TempDir()
			stateDir := filepath.Join(tmp, ".dir2mcp")
			dest := filepath.Join(tmp, "bundle.tar.gz")

			// A bundle left behind by an earlier run, or simply `touch`ed by the
			// operator, exactly as the issue reproduces it.
			if err := os.WriteFile(dest, []byte("stale bundle"), seedMode); err != nil {
				t.Fatalf("seed destination: %v", err)
			}
			if err := os.Chmod(dest, seedMode); err != nil {
				t.Fatalf("chmod seed: %v", err)
			}
			if got := modeOf(t, dest); got != seedMode {
				t.Fatalf("seed did not reproduce the reported state: destination is %04o, want %04o", got, seedMode)
			}

			runSupportBundleTo(t, stateDir, dest)

			if got := modeOf(t, dest); got != statefs.FileMode {
				t.Fatalf("overwritten bundle is %04o, want %04o: server.log and diagnostics stay readable by other local accounts", got, statefs.FileMode)
			}
		})
	}
}

// TestSupportBundleCreatesAnOwnerOnlyDestination retains the new-file guarantee
// the issue asks to keep, under the same permissive umask.
func TestSupportBundleCreatesAnOwnerOnlyDestination(t *testing.T) {
	withPermissiveUmask(t)
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "fresh.tar.gz")

	runSupportBundleTo(t, filepath.Join(tmp, ".dir2mcp"), dest)

	if got := modeOf(t, dest); got != statefs.FileMode {
		t.Fatalf("newly created bundle is %04o, want %04o", got, statefs.FileMode)
	}
}

// TestSupportBundleLeavesNoStrayTemporaries pins that the temp+rename flow
// cleans up after itself: a state directory littered with half-written
// archives would be its own disclosure.
func TestSupportBundleLeavesNoStrayTemporaries(t *testing.T) {
	withPermissiveUmask(t)
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "bundle.tar.gz")

	runSupportBundleTo(t, filepath.Join(tmp, ".dir2mcp"), dest)

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "bundle.tar.gz" && e.Name() != ".dir2mcp" {
			t.Fatalf("support-bundle left %q behind", e.Name())
		}
	}
}

// TestSupportBundleFailureDoesNotDestroyAnExistingBundle covers the second half
// of the issue's expected behaviour: the write is atomic, so a failure cannot
// leave the destination truncated. The pre-fix code opened the destination
// O_TRUNC before writing a single byte, so any later failure destroyed a
// previously valid bundle.
//
// The failure is induced by removing write permission on the containing
// directory, which blocks creating the temporary file.
func TestSupportBundleFailureDoesNotDestroyAnExistingBundle(t *testing.T) {
	withPermissiveUmask(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dest := filepath.Join(outDir, "bundle.tar.gz")
	const previous = "a previously valid bundle"
	if err := os.WriteFile(dest, []byte(previous), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	if code := app.Run([]string{"--state-dir", filepath.Join(tmp, ".dir2mcp"), "support-bundle", "--output", dest}); code == 0 {
		t.Fatalf("support-bundle unexpectedly succeeded writing into a read-only directory")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("previous bundle gone: %v", err)
	}
	if string(got) != previous {
		t.Fatalf("a failed write clobbered the previous bundle: got %q, want %q", got, previous)
	}
}
