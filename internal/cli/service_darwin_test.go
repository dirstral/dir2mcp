//go:build darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedCmd struct {
	name string
	args []string
}

func newFakeLaunchd(t *testing.T, output string, runErr error) (*launchdManager, *[]recordedCmd) {
	t.Helper()
	home := t.TempDir()
	calls := &[]recordedCmd{}
	mgr := &launchdManager{
		uid:  501,
		home: home,
		runCmd: func(name string, args ...string) (string, error) {
			*calls = append(*calls, recordedCmd{name: name, args: args})
			return output, runErr
		},
	}
	return mgr, calls
}

func TestLaunchdManager_InstallWritesPlistAndBootstraps(t *testing.T) {
	mgr, calls := newFakeLaunchd(t, "", nil)
	spec := serviceSpec{
		Label:      "com.dirstral.dir2mcp-demo-abc123",
		BinaryPath: "/usr/local/bin/dir2mcp",
		WorkingDir: "/Users/me/legal",
		Args:       []string{"up", "--foreground"},
		LogPath:    "/Users/me/legal/.dir2mcp/service.log",
	}

	unitPath, err := mgr.install(spec)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	wantPath := filepath.Join(mgr.home, "Library", "LaunchAgents", spec.Label+".plist")
	if unitPath != wantPath {
		t.Errorf("unitPath = %q, want %q", unitPath, wantPath)
	}
	body, readErr := os.ReadFile(wantPath)
	if readErr != nil {
		t.Fatalf("plist not written: %v", readErr)
	}
	if !strings.Contains(string(body), spec.Label) {
		t.Errorf("plist missing label:\n%s", body)
	}

	wantSeq := []string{"bootout", "bootstrap", "kickstart"}
	if len(*calls) != len(wantSeq) {
		t.Fatalf("got %d launchctl calls, want %d: %+v", len(*calls), len(wantSeq), *calls)
	}
	for i, want := range wantSeq {
		c := (*calls)[i]
		if c.name != "launchctl" || len(c.args) == 0 || c.args[0] != want {
			t.Errorf("call %d = %+v, want launchctl %s ...", i, c, want)
		}
	}
}

func TestLaunchdManager_InstallSurfacesBootstrapFailure(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "Bootstrap failed: 5: Input/output error", fmt.Errorf("exit status 5"))
	// bootout is best-effort (error ignored), but bootstrap failure must
	// surface. The fake returns an error for every call, so bootstrap fails.
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x"})
	if err == nil {
		t.Fatal("expected bootstrap failure to surface")
	}
	if !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("error should mention bootstrap: %v", err)
	}
}

func TestLaunchdManager_UninstallRemovesPlist(t *testing.T) {
	mgr, calls := newFakeLaunchd(t, "", nil)
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := mgr.plistPath(label)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	_, removed, err := mgr.uninstall(label)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !removed {
		t.Error("expected removed=true for an existing plist")
	}
	if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
		t.Errorf("plist still present after uninstall: %v", statErr)
	}
	if len(*calls) != 1 || (*calls)[0].args[0] != "bootout" {
		t.Errorf("expected a single bootout call, got %+v", *calls)
	}
}

func TestLaunchdManager_UninstallAbsentIsNoop(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "", nil)
	_, removed, err := mgr.uninstall("com.dirstral.not-installed")
	if err != nil {
		t.Fatalf("uninstall absent: %v", err)
	}
	if removed {
		t.Error("expected removed=false when no plist exists")
	}
}

func TestLaunchdManager_StatusReportsRunning(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "service = {\n\tstate = running\n}\n", nil)
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := mgr.plistPath(label)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	state, err := mgr.status(label)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !state.Installed || !state.Running {
		t.Errorf("expected installed+running, got %+v", state)
	}
}

// TestLaunchdManager_StatusReportsNotInstalled pins the "service absent"
// path: launchctl exits non-zero AND prints "Could not find service",
// which is the known-absent phrase the status method uses to distinguish
// a clean absent state from a real command failure.
func TestLaunchdManager_StatusReportsNotInstalled(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "Could not find service \"com.dirstral.absent\" in domain for port", fmt.Errorf("exit status 113"))
	state, err := mgr.status("com.dirstral.absent")
	if err != nil {
		t.Fatalf("status should not error for a known-absent service: %v", err)
	}
	if state.Installed || state.Running {
		t.Errorf("expected absent service, got %+v", state)
	}
}

// TestLaunchdManager_StatusErrorsOnCommandFailure pins that a launchctl
// failure whose output does not match a known-absent phrase is surfaced
// as an error rather than silently reported as "not installed".
func TestLaunchdManager_StatusErrorsOnCommandFailure(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "permission denied", fmt.Errorf("exit status 1"))
	_, err := mgr.status("com.dirstral.some-label")
	if err == nil {
		t.Fatal("expected status to surface non-absent launchctl failure as error")
	}
	if !strings.Contains(err.Error(), "launchctl print") {
		t.Errorf("error should mention launchctl print: %v", err)
	}
}

// TestLaunchdManager_PlistPathClampsTraversal verifies the filepath.Base
// defense-in-depth in plistPath: even if a label containing separators
// somehow reaches the method, the returned path stays inside LaunchAgents.
func TestLaunchdManager_PlistPathClampsTraversal(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, "", nil)
	crafted := "../../etc/evil"
	got := mgr.plistPath(crafted)
	base := filepath.Base(got)
	launchAgentsDir := filepath.Join(mgr.home, "Library", "LaunchAgents")
	if !strings.HasPrefix(got, launchAgentsDir) {
		t.Errorf("plistPath escaped LaunchAgents dir: %q", got)
	}
	if base != "evil.plist" {
		t.Errorf("expected clamped basename evil.plist, got %q", base)
	}
}
