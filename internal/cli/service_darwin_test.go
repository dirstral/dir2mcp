//go:build darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFakeLaunchd builds a launchd manager whose launchctl invocations are
// scripted per subcommand (see scriptedRunner). A subcommand with no scripted
// result succeeds silently.
func newFakeLaunchd(t *testing.T, script map[string][]cmdResult) (*launchdManager, *scriptedRunner) {
	t.Helper()
	runner := newScriptedRunner(script)
	return &launchdManager{uid: 501, home: t.TempDir(), runCmd: runner.run}, runner
}

// seedPlist writes a placeholder definition at the manager's plist path for
// label and returns the path.
func seedPlist(t *testing.T, mgr *launchdManager, label, body string) string {
	t.Helper()
	path := mgr.plistPath(label)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}
	return path
}

func TestLaunchdManager_InstallWritesPlistAndBootstraps(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, nil)
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
	if len(runner.calls) != len(wantSeq) {
		t.Fatalf("got %d launchctl calls, want %d: %v", len(runner.calls), len(wantSeq), runner.lines())
	}
	for i, want := range wantSeq {
		c := runner.calls[i]
		if c.name != "launchctl" || len(c.args) == 0 || c.args[0] != want {
			t.Errorf("call %d = %+v, want launchctl %s ...", i, c, want)
		}
	}
}

func TestLaunchdManager_InstallSurfacesBootstrapFailure(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{
		"bootstrap": {{out: "Bootstrap failed: 5: Input/output error", err: fmt.Errorf("exit status 5")}},
	})
	_, err := mgr.install(serviceSpec{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x"})
	if err == nil {
		t.Fatal("expected bootstrap failure to surface")
	}
	if !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("error should mention bootstrap: %v", err)
	}
}

func TestLaunchdManager_UninstallRemovesPlist(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, nil)
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := seedPlist(t, mgr, label, "<plist/>")

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
	if len(runner.calls) != 1 || runner.calls[0].args[0] != "bootout" {
		t.Errorf("expected a single bootout call, got %v", runner.lines())
	}
}

func TestLaunchdManager_UninstallAbsentIsNoop(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, nil)
	_, removed, err := mgr.uninstall("com.dirstral.not-installed")
	if err != nil {
		t.Fatalf("uninstall absent: %v", err)
	}
	if removed {
		t.Error("expected removed=false when no plist exists")
	}
}

func TestLaunchdManager_StatusReportsRunning(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{
		"print": {ok("service = {\n\tstate = running\n}\n")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	seedPlist(t, mgr, label, "<plist/>")

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
	mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{
		"print": {{out: "Could not find service \"com.dirstral.absent\" in domain for port", err: fmt.Errorf("exit status 113")}},
	})
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
	mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{"print": {fails("permission denied")}})
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
	mgr, _ := newFakeLaunchd(t, nil)
	crafted := "../../etc/evil"
	got := mgr.plistPath(crafted)
	base := filepath.Base(got)
	launchAgentsDir := filepath.Join(mgr.home, "Library", "LaunchAgents")
	if filepath.Dir(got) != launchAgentsDir {
		t.Errorf("plistPath escaped LaunchAgents dir: %q (want parent %s)", got, launchAgentsDir)
	}
	if base != "evil.plist" {
		t.Errorf("expected clamped basename evil.plist, got %q", base)
	}
}

// --- issue #723: uninstall must not orphan a running daemon ---

const priorPlistBody = "<plist><!-- previous definition --></plist>"

// TestLaunchdManager_UninstallKeepsPlistOnBootoutFailure_723 pins the fix: a
// bootout failure that is NOT a positively identified "already unloaded" result
// aborts the uninstall with the plist intact. Before the fix every bootout error
// was discarded, so a permission failure deleted the definition of a daemon that
// kept running, with nothing left to retry from.
func TestLaunchdManager_UninstallKeepsPlistOnBootoutFailure_723(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, map[string][]cmdResult{
		"bootout": {fails("Boot-out failed: 1: Operation not permitted")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := seedPlist(t, mgr, label, priorPlistBody)

	_, removed, err := mgr.uninstall(label)
	if err == nil {
		t.Fatal("expected a bootout failure to abort the uninstall")
	}
	if removed {
		t.Error("removed must be false when the service could not be unloaded")
	}
	if _, statErr := os.Stat(plistPath); statErr != nil {
		t.Errorf("the plist must be kept so the operator can retry: %v", statErr)
	}
	for _, want := range []string{"bootout", "still be running", plistPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if runner.count("bootout") != 1 {
		t.Errorf("expected exactly one bootout attempt: %v", runner.lines())
	}
}

// TestLaunchdManager_UninstallNotLoadedStaysIdempotent_723 pins that the
// known-absent results stay benign: uninstalling a service that is not booted in
// still removes the plist and reports success.
func TestLaunchdManager_UninstallNotLoadedStaysIdempotent_723(t *testing.T) {
	for _, out := range []string{
		"Boot-out failed: 3: No such process",
		"Boot-out failed: 2: No such file or directory",
		`Could not find service "com.dirstral.dir2mcp-demo-abc123" in domain for port`,
	} {
		mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{"bootout": {fails(out)}})
		label := "com.dirstral.dir2mcp-demo-abc123"
		plistPath := seedPlist(t, mgr, label, priorPlistBody)

		_, removed, err := mgr.uninstall(label)
		if err != nil {
			t.Fatalf("out=%q: uninstall should succeed for a not-loaded service: %v", out, err)
		}
		if !removed {
			t.Errorf("out=%q: expected removed=true", out)
		}
		if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
			t.Errorf("out=%q: plist should have been removed: %v", out, statErr)
		}
	}
}

// --- issue #724: a failed install must not strand or stop the previous service ---

// TestLaunchdManager_InstallRollsBackPriorServiceOnBootstrapFailure_724 is the
// core reinstall case: bootout succeeds (the previous service is stopped) and
// bootstrap then fails. Before the fix the operator was left with the previous
// daemon STOPPED and its plist overwritten by the replacement.
func TestLaunchdManager_InstallRollsBackPriorServiceOnBootstrapFailure_724(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, map[string][]cmdResult{
		// print succeeds => the previous service was loaded.
		"print": {ok("service = {\n\tstate = running\n}\n")},
		// The install's bootstrap fails; the rollback's re-bootstrap succeeds.
		"bootstrap": {fails("Bootstrap failed: 5: Input/output error"), ok("")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := seedPlist(t, mgr, label, priorPlistBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the bootstrap failure to surface")
	}
	body, readErr := os.ReadFile(plistPath)
	if readErr != nil {
		t.Fatalf("read plist: %v", readErr)
	}
	if string(body) != priorPlistBody {
		t.Errorf("the previous plist was not restored:\n%s", body)
	}
	if runner.count("bootstrap") != 2 {
		t.Errorf("the previously loaded service should have been booted back in: %v", runner.lines())
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should state the rollback: %v", err)
	}
	if strings.Contains(err.Error(), "ROLLBACK INCOMPLETE") {
		t.Errorf("rollback should have been complete: %v", err)
	}
}

// TestLaunchdManager_InstallRollsBackOnKickstartFailure_724 covers the last
// step: kickstart failed, so the replacement may already be loaded. It must be
// booted out and the previous definition put back.
func TestLaunchdManager_InstallRollsBackOnKickstartFailure_724(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, map[string][]cmdResult{
		"print":     {ok("service = {\n\tstate = running\n}\n")},
		"kickstart": {fails("Could not kickstart service")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := seedPlist(t, mgr, label, priorPlistBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the kickstart failure to surface")
	}
	body, _ := os.ReadFile(plistPath)
	if string(body) != priorPlistBody {
		t.Errorf("the previous plist was not restored:\n%s", body)
	}
	// bootout: once for the reload, once to unload the failed replacement.
	if runner.count("bootout") != 2 {
		t.Errorf("the replacement should have been booted out: %v", runner.lines())
	}
	if runner.count("bootstrap") != 2 {
		t.Errorf("the previous service should have been booted back in: %v", runner.lines())
	}
}

// TestLaunchdManager_InstallKeepsPriorServiceOnBootoutFailure_724 covers the
// case where nothing was unloaded: the previous service is still live on its
// in-memory definition, so the rollback restores the file and must NOT bootstrap
// anything on top of it.
func TestLaunchdManager_InstallKeepsPriorServiceOnBootoutFailure_724(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, map[string][]cmdResult{
		"print":   {ok("service = {\n\tstate = running\n}\n")},
		"bootout": {fails("Boot-out failed: 1: Operation not permitted")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath := seedPlist(t, mgr, label, priorPlistBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the bootout failure to surface")
	}
	body, _ := os.ReadFile(plistPath)
	if string(body) != priorPlistBody {
		t.Errorf("the previous plist was not restored:\n%s", body)
	}
	if runner.count("bootstrap") != 0 {
		t.Errorf("nothing should have been bootstrapped over a still-loaded service: %v", runner.lines())
	}
	if runner.count("kickstart") != 0 {
		t.Errorf("install must stop at the failed bootout: %v", runner.lines())
	}
}

// TestLaunchdManager_InstallCleansUpFailedFirstInstall_724 pins the first-time
// case: a failed install must leave NO plist behind, so a half-installed service
// cannot activate at the next login.
func TestLaunchdManager_InstallCleansUpFailedFirstInstall_724(t *testing.T) {
	mgr, runner := newFakeLaunchd(t, map[string][]cmdResult{
		"bootstrap": {fails("Bootstrap failed: 5: Input/output error")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	plistPath, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the bootstrap failure to surface")
	}
	if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
		t.Errorf("a failed first install must not leave a plist behind: %v", statErr)
	}
	if !strings.Contains(err.Error(), "no service definition was left") {
		t.Errorf("error should state that nothing was left installed: %v", err)
	}
	// No previous service existed, so nothing may be bootstrapped back.
	if runner.count("bootstrap") != 1 {
		t.Errorf("rollback must not bootstrap a service that never existed: %v", runner.lines())
	}
	// A never-loaded prior service is not probed with print.
	if runner.count("print") != 0 {
		t.Errorf("a first install should not probe the previous service: %v", runner.lines())
	}
}

// TestLaunchdManager_InstallReportsIncompleteRollback_724 pins the honesty
// requirement: when the rollback itself fails, the operator is told exactly what
// could not be restored instead of getting a bare install error.
func TestLaunchdManager_InstallReportsIncompleteRollback_724(t *testing.T) {
	mgr, _ := newFakeLaunchd(t, map[string][]cmdResult{
		"print":     {ok("service = {\n\tstate = running\n}\n")},
		"bootstrap": {fails("Bootstrap failed: 5: Input/output error"), fails("Bootstrap failed: 1: Operation not permitted")},
	})
	label := "com.dirstral.dir2mcp-demo-abc123"
	seedPlist(t, mgr, label, priorPlistBody)

	_, err := mgr.install(serviceSpec{Label: label, BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l.log"})
	if err == nil {
		t.Fatal("expected the install failure to surface")
	}
	if !strings.Contains(err.Error(), "ROLLBACK INCOMPLETE") {
		t.Errorf("an unrecoverable rollback must be explicit: %v", err)
	}
	if !strings.Contains(err.Error(), "no longer loaded") {
		t.Errorf("error should say the previous service is down: %v", err)
	}
}
