//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemdManager supervises a per-corpus dir2mcp daemon as a systemd user
// service. runCmd is injectable so the daemon-reload/enable/status sequence
// can be unit-tested without touching a real systemd. It mirrors the macOS
// launchdManager structure.
type systemdManager struct {
	home   string
	runCmd func(name string, args ...string) (string, error)
}

// newServiceManager returns the Linux systemd user-service backend.
func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return &systemdManager{
		home: home,
		runCmd: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
	}, nil
}

// backendName identifies this manager in CLI output.
func (m *systemdManager) backendName() string { return "systemd" }

// unitPath returns the canonical user-unit path for label. filepath.Base
// clamps any residual path separators so the unit cannot escape the systemd
// user directory even if upstream validation is somehow bypassed.
func (m *systemdManager) unitPath(label string) string {
	return filepath.Join(m.home, ".config", "systemd", "user", filepath.Base(label)+".service")
}

// install writes the user unit, reloads the daemon, and enables+starts it.
func (m *systemdManager) install(spec serviceSpec) (string, error) {
	if err := rejectMultilineSpec(spec); err != nil {
		return "", err
	}
	unit := m.unitPath(spec.Label)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", fmt.Errorf("create systemd user dir: %w", err)
	}
	if err := os.WriteFile(unit, []byte(renderSystemdUnit(spec)), 0o644); err != nil {
		return unit, fmt.Errorf("write unit %s: %w", unit, err)
	}
	if out, err := m.runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return unit, fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := m.runCmd("systemctl", "--user", "enable", "--now", filepath.Base(unit)); err != nil {
		return unit, fmt.Errorf("systemctl enable --now: %w: %s", err, strings.TrimSpace(out))
	}
	return unit, nil
}

// uninstall disables+stops the unit, removes the file, and reloads the
// daemon. Idempotent: removed=false when the unit file was already absent.
func (m *systemdManager) uninstall(label string) (string, bool, error) {
	unit := m.unitPath(label)
	// Best-effort disable+stop (ignore the error when the unit is not loaded),
	// then remove the file so it won't re-enable at next login.
	_, _ = m.runCmd("systemctl", "--user", "disable", "--now", filepath.Base(unit))
	_, err := os.Stat(unit)
	if err != nil {
		if os.IsNotExist(err) {
			return unit, false, nil
		}
		return unit, false, fmt.Errorf("stat unit %s: %w", unit, err)
	}
	if err := os.Remove(unit); err != nil {
		return unit, false, fmt.Errorf("remove unit %s: %w", unit, err)
	}
	if out, err := m.runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return unit, true, fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(out))
	}
	return unit, true, nil
}

// status reports whether the unit is installed (file on disk / enabled) and
// running (active). is-active/is-enabled exit non-zero for the normal
// inactive/disabled states, so their errors are best-effort ignored and the
// verdict is read from stdout ("active"/"enabled"/...); the on-disk unit file
// is the authoritative installed signal.
func (m *systemdManager) status(label string) (serviceState, error) {
	unit := m.unitPath(label)
	state := serviceState{UnitPath: unit}
	if _, err := os.Stat(unit); err == nil {
		state.Installed = true
	} else if !os.IsNotExist(err) {
		return state, fmt.Errorf("stat unit %s: %w", unit, err)
	}

	base := filepath.Base(unit)
	activeOut, _ := m.runCmd("systemctl", "--user", "is-active", base)
	state.Running = strings.TrimSpace(activeOut) == "active"

	enabledOut, _ := m.runCmd("systemctl", "--user", "is-enabled", base)
	enabled := strings.TrimSpace(enabledOut)
	if enabled == "enabled" || enabled == "static" || enabled == "linked" {
		state.Installed = true
	}

	switch {
	case state.Running:
		state.Detail = "running"
	case state.Installed:
		state.Detail = "installed, not running"
	default:
		state.Detail = "not installed"
	}
	return state, nil
}
