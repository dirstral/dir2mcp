//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFakeSystemd builds a systemd manager whose systemctl invocations are
// scripted per subcommand (see scriptedRunner). The is-active/is-enabled probes
// default to the verdicts a real systemd gives for a unit it does not know, so
// the happy path only has to script what it wants to fail.
func newFakeSystemd(t *testing.T, script map[string][]cmdResult) (*systemdManager, *scriptedRunner) {
	t.Helper()
	merged := map[string][]cmdResult{
		"is-active":  {fails("inactive\n")},
		"is-enabled": {fails("not-found\n")},
	}
	for k, v := range script {
		merged[k] = v
	}
	runner := newScriptedRunner(merged)
	return &systemdManager{home: t.TempDir(), runCmd: runner.run}, runner
}

// seedUnit writes a placeholder unit at the manager's unit path for label.
func seedUnit(t *testing.T, mgr *systemdManager, label, body string) string {
	t.Helper()
	unit := mgr.unitPath(label)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		t.Fatalf("seed unit: %v", err)
	}
	return unit
}

func TestSystemdManager_InstallWritesUnitAndEnables(t *testing.T) {
	mgr, runner := newFakeSystemd(t, nil)
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

	// The is-active/is-enabled probes capture the prior supervisor state before
	// anything is mutated (#724), so a failed step can put it back.
	want := []string{
		"systemctl --user is-active com.dirstral.dir2mcp-demo-abc123.service",
		"systemctl --user is-enabled com.dirstral.dir2mcp-demo-abc123.service",
		"systemctl --user daemon-reload",
		"systemctl --user enable --now com.dirstral.dir2mcp-demo-abc123.service",
	}
	got := runner.lines()
	if len(got) != len(want) {
		t.Fatalf("got %d systemctl calls, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestSystemdManager_InstallSurfacesEnableFailure(t *testing.T) {
	mgr, _ := newFakeSystemd(t, map[string][]cmdResult{
		"enable": {fails("Failed to enable unit")},
	})
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected enable/daemon-reload failure to surface")
	}
}

func TestSystemdManager_InstallRejectsNewlineValue(t *testing.T) {
	mgr, runner := newFakeSystemd(t, nil)
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x\ninjected=1", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected a newline-in-value rejection")
	}
	if len(runner.calls) != 0 {
		t.Errorf("no systemctl call should run when the spec is rejected, got %v", runner.lines())
	}
}

func TestSystemdManager_UninstallRemovesUnit(t *testing.T) {
	mgr, runner := newFakeSystemd(t, nil)
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := seedUnit(t, mgr, label, "[Unit]\n")

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
	got := runner.lines()
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestSystemdManager_UninstallAbsentIsIdempotent(t *testing.T) {
	mgr, runner := newFakeSystemd(t, nil)
	_, removed, err := mgr.uninstall("com.dirstral.not-installed")
	if err != nil {
		t.Fatalf("uninstall absent: %v", err)
	}
	if removed {
		t.Error("expected removed=false when no unit exists")
	}
	// Only the disable ran; no file to remove, no reload needed.
	if len(runner.calls) != 1 || runner.lines()[0] != "systemctl --user disable --now com.dirstral.not-installed.service" {
		t.Errorf("expected a single disable call, got %v", runner.lines())
	}
}

func TestSystemdManager_StatusReportsRunning(t *testing.T) {
	mgr, _ := newFakeSystemd(t, map[string][]cmdResult{
		"is-active":  {ok("active\n")},
		"is-enabled": {ok("enabled\n")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	seedUnit(t, mgr, label, "[Unit]\n")

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
	mgr, _ := newFakeSystemd(t, nil)
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
	mgr, _ := newFakeSystemd(t, nil)
	got := mgr.unitPath("../../etc/evil")
	wantDir := filepath.Join(mgr.home, ".config", "systemd", "user")
	if filepath.Dir(got) != wantDir {
		t.Errorf("unitPath escaped user dir: %q (want parent %s)", got, wantDir)
	}
	if filepath.Base(got) != "evil.service" {
		t.Errorf("expected clamped basename evil.service, got %q", filepath.Base(got))
	}
}

// --- issue #725: a systemctl that cannot answer is not a stopped service ---

// TestSystemdManager_StatusFailsWhenSystemctlCannotAnswer_725 pins the fix: a
// dead user bus, a permission denial, or a missing systemctl must surface as an
// operational error. Before the fix both is-active and is-enabled errors were
// discarded, so status derived "installed: true, running: false" from the
// on-disk unit file alone — a state snapshot the tool had not actually measured.
func TestSystemdManager_StatusFailsWhenSystemctlCannotAnswer_725(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script map[string][]cmdResult
	}{
		{"dead user bus", map[string][]cmdResult{"is-active": {fails("Failed to connect to bus: No such file or directory\n")}}},
		{"no session bus", map[string][]cmdResult{"is-active": {fails("Failed to connect to bus: No medium found\n")}}},
		{"permission denied on is-enabled", map[string][]cmdResult{"is-enabled": {fails("Failed to get unit file state: Permission denied\n")}}},
		{"systemctl missing", map[string][]cmdResult{"is-active": {{err: fmt.Errorf(`exec: "systemctl": executable file not found in $PATH`)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _ := newFakeSystemd(t, tc.script)
			label := "com.dirstral.dir2mcp-demo-abc123"
			seedUnit(t, mgr, label, "[Unit]\n")

			state, err := mgr.status(label)
			if err == nil {
				t.Fatalf("expected an operational error, got state %+v", state)
			}
			if state.Running {
				t.Errorf("a failed probe must never report running: %+v", state)
			}
		})
	}
}

// TestSystemdManager_StatusKeepsDocumentedStatesNormal_725 pins the other side:
// the documented inactive/failed/disabled/not-found verdicts still exit non-zero
// and must stay ordinary state, not errors.
func TestSystemdManager_StatusKeepsDocumentedStatesNormal_725(t *testing.T) {
	for _, tc := range []struct {
		active, enabled string
		wantRunning     bool
		wantInstalled   bool
	}{
		{"inactive\n", "disabled\n", false, true},
		{"failed\n", "enabled\n", false, true},
		{"active\n", "static\n", true, true},
		{"activating\n", "enabled-runtime\n", false, true},
	} {
		mgr, _ := newFakeSystemd(t, map[string][]cmdResult{
			"is-active":  {fails(tc.active)},
			"is-enabled": {fails(tc.enabled)},
		})
		label := "com.dirstral.dir2mcp-demo-abc123"
		seedUnit(t, mgr, label, "[Unit]\n")

		state, err := mgr.status(label)
		if err != nil {
			t.Fatalf("active=%q enabled=%q: status should not error: %v", tc.active, tc.enabled, err)
		}
		if state.Running != tc.wantRunning || state.Installed != tc.wantInstalled {
			t.Errorf("active=%q enabled=%q: got %+v", tc.active, tc.enabled, state)
		}
	}
}

// --- issue #723: uninstall must not orphan a running daemon ---

// TestSystemdManager_UninstallKeepsUnitOnDisableFailure_723 pins the fix: a
// disable failure that is NOT a positively identified absent unit aborts the
// uninstall with the unit file intact. Before the fix the error was discarded
// and the unit was deleted while its daemon kept running, leaving dangling
// enablement symlinks and nothing to retry from.
func TestSystemdManager_UninstallKeepsUnitOnDisableFailure_723(t *testing.T) {
	for _, out := range []string{
		"Failed to connect to bus: No medium found\n",
		"Failed to disable unit: Access denied\n",
		"Interactive authentication required.\n",
	} {
		mgr, runner := newFakeSystemd(t, map[string][]cmdResult{"disable": {fails(out)}})
		label := "com.dirstral.dir2mcp-demo-abc123"
		unit := seedUnit(t, mgr, label, "[Unit]\n")

		_, removed, err := mgr.uninstall(label)
		if err == nil {
			t.Fatalf("out=%q: expected the disable failure to abort the uninstall", out)
		}
		if removed {
			t.Errorf("out=%q: removed must be false when the unit could not be stopped", out)
		}
		if _, statErr := os.Stat(unit); statErr != nil {
			t.Errorf("out=%q: the unit must be kept so the operator can retry: %v", out, statErr)
		}
		if !strings.Contains(err.Error(), "still be running") {
			t.Errorf("out=%q: error should warn the unit may still run: %v", out, err)
		}
		if runner.count("daemon-reload") != 0 {
			t.Errorf("out=%q: uninstall must stop at the failed disable: %v", out, runner.lines())
		}
	}
}

// TestSystemdManager_UninstallAbsentUnitStaysIdempotent_723 pins that a
// positively identified absent/not-loaded unit remains benign.
func TestSystemdManager_UninstallAbsentUnitStaysIdempotent_723(t *testing.T) {
	for _, out := range []string{
		"Failed to disable unit: Unit file com.dirstral.dir2mcp-demo-abc123.service does not exist.\n",
		"Failed to stop com.dirstral.dir2mcp-demo-abc123.service: Unit com.dirstral.dir2mcp-demo-abc123.service not loaded.\n",
	} {
		mgr, _ := newFakeSystemd(t, map[string][]cmdResult{"disable": {fails(out)}})
		label := "com.dirstral.dir2mcp-demo-abc123"
		unit := seedUnit(t, mgr, label, "[Unit]\n")

		_, removed, err := mgr.uninstall(label)
		if err != nil {
			t.Fatalf("out=%q: uninstall should succeed for an absent unit: %v", out, err)
		}
		if !removed {
			t.Errorf("out=%q: expected removed=true", out)
		}
		if _, statErr := os.Stat(unit); !os.IsNotExist(statErr) {
			t.Errorf("out=%q: unit should have been removed: %v", out, statErr)
		}
	}
}

// --- issue #724: a failed install must not strand or stop the previous unit ---

const priorUnitBody = "[Service]\nExecStart=/old/dir2mcp up --foreground\n"

// TestSystemdManager_InstallRestoresPriorUnitOnEnableFailure_724 is the core
// reinstall case: the unit was overwritten and `enable --now` then failed.
// Before the fix the new unit stayed on disk (possibly partially enabled) with
// no restoration of the previous definition or its enabled+running state.
func TestSystemdManager_InstallRestoresPriorUnitOnEnableFailure_724(t *testing.T) {
	mgr, runner := newFakeSystemd(t, map[string][]cmdResult{
		"is-active":  {fails("active\n")},
		"is-enabled": {ok("enabled\n")},
		"enable":     {fails("Failed to enable unit: Invalid argument"), ok("")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := seedUnit(t, mgr, label, priorUnitBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the enable failure to surface")
	}
	body, readErr := os.ReadFile(unit)
	if readErr != nil {
		t.Fatalf("read unit: %v", readErr)
	}
	if string(body) != priorUnitBody {
		t.Errorf("the previous unit was not restored:\n%s", body)
	}
	// The partial `enable --now` is reversed, then the prior unit is re-enabled.
	if runner.count("disable") != 1 {
		t.Errorf("the partially enabled replacement should have been disabled: %v", runner.lines())
	}
	if runner.count("enable") != 2 {
		t.Errorf("the previously enabled+active unit should have been re-enabled: %v", runner.lines())
	}
	if runner.count("daemon-reload") != 2 {
		t.Errorf("systemd should be reloaded after restoring the unit: %v", runner.lines())
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should state the rollback: %v", err)
	}
	if strings.Contains(err.Error(), "ROLLBACK INCOMPLETE") {
		t.Errorf("rollback should have been complete: %v", err)
	}
}

// TestSystemdManager_InstallRestoresPriorUnitOnReloadFailure_724 covers the
// earlier step: daemon-reload failed, so nothing was enabled and the rollback
// only has to put the unit file back.
func TestSystemdManager_InstallRestoresPriorUnitOnReloadFailure_724(t *testing.T) {
	mgr, runner := newFakeSystemd(t, map[string][]cmdResult{
		"is-active":     {fails("inactive\n")},
		"is-enabled":    {ok("enabled\n")},
		"daemon-reload": {fails("Failed to connect to bus: No medium found"), ok("")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := seedUnit(t, mgr, label, priorUnitBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the daemon-reload failure to surface")
	}
	body, _ := os.ReadFile(unit)
	if string(body) != priorUnitBody {
		t.Errorf("the previous unit was not restored:\n%s", body)
	}
	if runner.count("disable") != 0 {
		t.Errorf("nothing was enabled, so nothing may be disabled: %v", runner.lines())
	}
	// Enabled but not active: re-enable without --now.
	lines := runner.lines()
	if lines[len(lines)-1] != "systemctl --user enable com.dirstral.dir2mcp-demo-abc123.service" {
		t.Errorf("an enabled-but-stopped unit must not be started by the rollback: %v", lines)
	}
}

// TestSystemdManager_InstallCleansUpFailedFirstInstall_724 pins the first-time
// case: a failed install must leave no unit behind, so it cannot activate at the
// next login, and must not try to re-enable a unit that never existed.
func TestSystemdManager_InstallCleansUpFailedFirstInstall_724(t *testing.T) {
	mgr, runner := newFakeSystemd(t, map[string][]cmdResult{
		"enable": {fails("Failed to enable unit: Invalid argument")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the enable failure to surface")
	}
	if _, statErr := os.Stat(unit); !os.IsNotExist(statErr) {
		t.Errorf("a failed first install must not leave a unit behind: %v", statErr)
	}
	if !strings.Contains(err.Error(), "no service definition was left") {
		t.Errorf("error should state that nothing was left installed: %v", err)
	}
	if runner.count("enable") != 1 {
		t.Errorf("rollback must not re-enable a unit that never existed: %v", runner.lines())
	}
	if runner.count("disable") != 1 {
		t.Errorf("the partial registration should have been disabled: %v", runner.lines())
	}
}

// TestSystemdManager_InstallFailsBeforeWritingWhenBusUnreachable_724 pins the
// strongest transactionality guarantee available here: when systemd cannot be
// reached at all, install fails with the previous definition untouched on disk,
// because the prior state is captured before anything is written.
func TestSystemdManager_InstallFailsBeforeWritingWhenBusUnreachable_724(t *testing.T) {
	mgr, runner := newFakeSystemd(t, map[string][]cmdResult{
		"is-active": {fails("Failed to connect to bus: No medium found\n")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	unit := seedUnit(t, mgr, label, priorUnitBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected an unreachable user manager to fail the install")
	}
	body, _ := os.ReadFile(unit)
	if string(body) != priorUnitBody {
		t.Errorf("the unit must not be touched when systemd cannot be reached:\n%s", body)
	}
	if runner.count("daemon-reload") != 0 || runner.count("enable") != 0 {
		t.Errorf("no mutation may be attempted: %v", runner.lines())
	}
}

// TestSystemdManager_InstallReportsIncompleteRollback_724 pins the honesty
// requirement: when the rollback cannot restore the previous unit, the operator
// is told exactly what is still wrong.
func TestSystemdManager_InstallReportsIncompleteRollback_724(t *testing.T) {
	mgr, _ := newFakeSystemd(t, map[string][]cmdResult{
		"is-active":  {fails("active\n")},
		"is-enabled": {ok("enabled\n")},
		"enable":     {fails("Failed to enable unit: Invalid argument"), fails("Failed to enable unit: Access denied")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	seedUnit(t, mgr, label, priorUnitBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the install failure to surface")
	}
	if !strings.Contains(err.Error(), "ROLLBACK INCOMPLETE") {
		t.Errorf("an unrecoverable rollback must be explicit: %v", err)
	}
	if !strings.Contains(err.Error(), "could not re-enable and restart the previous unit") {
		t.Errorf("error should name what could not be restored: %v", err)
	}
}
