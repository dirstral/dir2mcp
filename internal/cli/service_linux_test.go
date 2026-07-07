//go:build linux

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

func newFakeSystemd(t *testing.T, output string, runErr error) (*systemdManager, *[]recordedCmd) {
	t.Helper()
	home := t.TempDir()
	calls := &[]recordedCmd{}
	mgr := &systemdManager{
		home: home,
		runCmd: func(name string, args ...string) (string, error) {
			*calls = append(*calls, recordedCmd{name: name, args: args})
			return output, runErr
		},
	}
	return mgr, calls
}

// argLine joins a recorded command back into a single string for sequence
// assertions.
func argLine(c recordedCmd) string {
	return c.name + " " + strings.Join(c.args, " ")
}

func TestSystemdManager_InstallWritesUnitAndEnables(t *testing.T) {
	mgr, calls := newFakeSystemd(t, "", nil)
	spec := serviceSpec{
		Label:      "com.dirstral.dir2mcp-demo-abc123",
		BinaryPath: "/usr/local/bin/dir2mcp",
		WorkingDir: "/srv/corpus",
		Args:       []string{"up", "--foreground"},
		LogPath:    "/srv/corpus/.dir2mcp/service.log",
	}

	unit, err := mgr.install(spec)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	wantPath := filepath.Join(mgr.home, ".config", "systemd", "user", spec.Label+".service")
	if unit != wantPath {
		t.Errorf("unit path = %q, want %q", unit, wantPath)
	}
	fi, statErr := os.Stat(wantPath)
	if statErr != nil {
		t.Fatalf("unit not written: %v", statErr)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("unit perms = %v, want 0644", fi.Mode().Perm())
	}
	body, _ := os.ReadFile(wantPath)
	if !strings.Contains(string(body), "Restart=on-failure") {
		t.Errorf("unit missing supervision block:\n%s", body)
	}

	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now com.dirstral.dir2mcp-demo-abc123.service",
	}
	if len(*calls) != len(want) {
		t.Fatalf("got %d systemctl calls, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := argLine((*calls)[i]); got != w {
			t.Errorf("call %d = %q, want %q", i, got, w)
		}
	}
}

func TestSystemdManager_InstallSurfacesEnableFailure(t *testing.T) {
	mgr, _ := newFakeSystemd(t, "Failed to enable unit", fmt.Errorf("exit status 1"))
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected enable/daemon-reload failure to surface")
	}
}

func TestSystemdManager_InstallRejectsNewlineValue(t *testing.T) {
	mgr, calls := newFakeSystemd(t, "", nil)
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x\ninjected=1", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected a newline-in-value rejection")
	}
	if len(*calls) != 0 {
		t.Errorf("no systemctl call should run when the spec is rejected, got %+v", *calls)
	}
}

func TestSystemdManager_UninstallRemovesUnit(t *testing.T) {
	mgr, calls := newFakeSystemd(t, "", nil)
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := mgr.unitPath(label)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	_, removed, err := mgr.uninstall(label)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !removed {
		t.Error("expected removed=true for an existing unit")
	}
	if _, statErr := os.Stat(unit); !os.IsNotExist(statErr) {
		t.Errorf("unit still present after uninstall: %v", statErr)
	}
	want := []string{
		"systemctl --user disable --now com.dirstral.dir2mcp-demo-abc123.service",
		"systemctl --user daemon-reload",
	}
	if len(*calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := argLine((*calls)[i]); got != w {
			t.Errorf("call %d = %q, want %q", i, got, w)
		}
	}
}

func TestSystemdManager_UninstallAbsentIsIdempotent(t *testing.T) {
	mgr, calls := newFakeSystemd(t, "", nil)
	_, removed, err := mgr.uninstall("com.dirstral.not-installed")
	if err != nil {
		t.Fatalf("uninstall absent: %v", err)
	}
	if removed {
		t.Error("expected removed=false when no unit exists")
	}
	// Only the best-effort disable ran; no file to remove, no reload needed.
	if len(*calls) != 1 || argLine((*calls)[0]) != "systemctl --user disable --now com.dirstral.not-installed.service" {
		t.Errorf("expected a single disable call, got %+v", *calls)
	}
}

func TestSystemdManager_StatusReportsRunning(t *testing.T) {
	// is-active -> "active", is-enabled -> "enabled".
	outputs := []string{"active\n", "enabled\n"}
	var i int
	mgr := &systemdManager{
		home: t.TempDir(),
		runCmd: func(_ string, _ ...string) (string, error) {
			o := outputs[i]
			i++
			return o, nil
		},
	}
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := mgr.unitPath(label)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	state, err := mgr.status(label)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !state.Installed || !state.Running {
		t.Errorf("expected installed+running, got %+v", state)
	}
}

func TestSystemdManager_StatusReportsNotInstalled(t *testing.T) {
	// is-active exits non-zero with "inactive"; is-enabled with "not-found".
	outputs := []string{"inactive\n", "not-found\n"}
	var i int
	mgr := &systemdManager{
		home: t.TempDir(),
		runCmd: func(_ string, _ ...string) (string, error) {
			o := outputs[i]
			i++
			return o, fmt.Errorf("exit status 1")
		},
	}
	state, err := mgr.status("com.dirstral.absent")
	if err != nil {
		t.Fatalf("status should not error for an absent unit: %v", err)
	}
	if state.Installed || state.Running {
		t.Errorf("expected absent unit, got %+v", state)
	}
	if state.Detail != "not installed" {
		t.Errorf("detail = %q, want \"not installed\"", state.Detail)
	}
}

func TestSystemdManager_UnitPathClampsTraversal(t *testing.T) {
	mgr, _ := newFakeSystemd(t, "", nil)
	got := mgr.unitPath("../../etc/evil")
	wantDir := filepath.Join(mgr.home, ".config", "systemd", "user")
	if filepath.Dir(got) != wantDir {
		t.Errorf("unitPath escaped user dir: %q (want parent %s)", got, wantDir)
	}
	if filepath.Base(got) != "evil.service" {
		t.Errorf("expected clamped basename evil.service, got %q", filepath.Base(got))
	}
}
