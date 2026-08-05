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

// priorUnitState is the supervisor state a failed install has to put back: the
// unit's enablement and whether it was running.
type priorUnitState struct {
	enabled bool
	active  bool
}

// install writes the user unit, reloads the daemon, and enables+starts it.
//
// It is transactional as far as systemd permits (#724):
//
//  1. the prior supervisor state is captured BEFORE anything is mutated, so an
//     unreachable user manager fails with the machine completely untouched;
//  2. the replacement unit is written atomically;
//  3. any failing systemctl step restores the prior unit file, reloads, and
//     re-establishes the prior enablement/running state — or, for a first-time
//     install, disables the partial registration and removes the unit so it
//     cannot activate at the next login;
//  4. anything the rollback could not undo is named in the returned error.
//
// What remains non-atomic: `enable --now` is two operations to systemd (link +
// start) with no combined undo, so a failure can leave the replacement briefly
// enabled or started. Rollback reverses it explicitly with `disable --now`; it
// cannot make the window not exist.
func (m *systemdManager) install(spec serviceSpec) (string, error) {
	if err := rejectMultilineSpec(spec); err != nil {
		return "", err
	}
	unit := m.unitPath(spec.Label)
	base := filepath.Base(unit)
	// systemd must be able to read this; it is a service definition, not
	// corpus-derived content (#726).
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", fmt.Errorf("create systemd user dir: %w", err)
	}
	txn, err := beginUnitTxn(unit, 0o644)
	if err != nil {
		return "", fmt.Errorf("read existing unit %s: %w", unit, err)
	}
	prior, err := m.captureUnitState(base)
	if err != nil {
		// Nothing has been written yet: fail with the machine untouched rather
		// than installing a unit we cannot enable or roll back.
		return unit, err
	}
	if err := txn.write(renderSystemdUnit(spec)); err != nil {
		return unit, fmt.Errorf("write unit %s: %w", unit, err)
	}
	if out, err := m.runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return unit, m.rollbackInstall(txn, base, false, prior,
			fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(out)))
	}
	if out, err := m.runCmd("systemctl", "--user", "enable", "--now", base); err != nil {
		return unit, m.rollbackInstall(txn, base, true, prior,
			fmt.Errorf("systemctl enable --now: %w: %s", err, strings.TrimSpace(out)))
	}
	return unit, nil
}

// captureUnitState records whether the unit is currently enabled and running.
// A systemctl invocation that cannot answer is an error, not an assumption.
func (m *systemdManager) captureUnitState(base string) (priorUnitState, error) {
	active, err := m.queryState(systemctlIsActive, base)
	if err != nil {
		return priorUnitState{}, err
	}
	enabled, err := m.queryState(systemctlIsEnabled, base)
	if err != nil {
		return priorUnitState{}, err
	}
	return priorUnitState{enabled: systemctlEnabledMeansInstalled(enabled), active: active == "active"}, nil
}

// queryState runs one `systemctl --user is-*` probe and classifies its result.
func (m *systemdManager) queryState(q systemctlQuery, base string) (string, error) {
	out, err := m.runCmd("systemctl", "--user", q.name, base)
	return q.classify(out, err)
}

// systemctlEnabledMeansInstalled reports whether an is-enabled verdict implies
// the unit is registered with systemd (and so "installed" for status purposes).
func systemctlEnabledMeansInstalled(verdict string) bool {
	switch verdict {
	case "enabled", "enabled-runtime", "static", "linked", "linked-runtime":
		return true
	default:
		return false
	}
}

// rollbackInstall undoes a partially applied install and returns cause annotated
// with the resulting state. disableNew reverses an `enable --now` that may have
// partially taken effect.
func (m *systemdManager) rollbackInstall(txn *unitTxn, base string, disableNew bool, prior priorUnitState, cause error) error {
	var problems []string
	if disableNew {
		if out, err := m.runCmd("systemctl", "--user", "disable", "--now", base); err != nil && !systemctlReportsAbsent(out) {
			problems = append(problems, fmt.Sprintf("could not disable the partially installed unit %s: %v: %s",
				base, err, strings.TrimSpace(out)))
		}
	}
	if err := txn.rollback(); err != nil {
		problems = append(problems, fmt.Sprintf("could not restore %s: %v", txn.path, err))
		return wrapInstallRollback(cause, txn, problems)
	}
	if out, err := m.runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		problems = append(problems, fmt.Sprintf("could not reload systemd after restoring %s: %v: %s",
			txn.path, err, strings.TrimSpace(out)))
	}
	if txn.had {
		problems = append(problems, m.restorePriorUnit(base, prior)...)
	}
	return wrapInstallRollback(cause, txn, problems)
}

// restorePriorUnit re-establishes the enablement/running state the unit had
// before the install, returning a description of anything it could not restore.
func (m *systemdManager) restorePriorUnit(base string, prior priorUnitState) []string {
	var problems []string
	switch {
	case prior.enabled && prior.active:
		if out, err := m.runCmd("systemctl", "--user", "enable", "--now", base); err != nil {
			problems = append(problems, fmt.Sprintf("could not re-enable and restart the previous unit %s: %v: %s", base, err, strings.TrimSpace(out)))
		}
	case prior.enabled:
		if out, err := m.runCmd("systemctl", "--user", "enable", base); err != nil {
			problems = append(problems, fmt.Sprintf("could not re-enable the previous unit %s: %v: %s", base, err, strings.TrimSpace(out)))
		}
	case prior.active:
		if out, err := m.runCmd("systemctl", "--user", "start", base); err != nil {
			problems = append(problems, fmt.Sprintf("could not restart the previous unit %s: %v: %s", base, err, strings.TrimSpace(out)))
		}
	}
	return problems
}

// uninstall disables+stops the unit, removes the file, and reloads the
// daemon. Idempotent: removed=false when the unit file was already absent.
//
// Only a positively identified "unit absent / not loaded" disable result counts
// as idempotent success (#723). A dead user bus, a permission denial, or a
// missing systemctl keeps the unit file on disk and returns an error: removing
// the definition of a unit that may still be running would orphan the daemon and
// leave dangling enablement symlinks with nothing left to retry from.
func (m *systemdManager) uninstall(label string) (string, bool, error) {
	unit := m.unitPath(label)
	base := filepath.Base(unit)
	if out, err := m.runCmd("systemctl", "--user", "disable", "--now", base); err != nil && !systemctlReportsAbsent(out) {
		return unit, false, fmt.Errorf(
			"systemctl --user disable --now %s: %w: %s (the unit may still be running; %s was kept so you can retry)",
			base, err, strings.TrimSpace(out), unit)
	}
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
// running (active).
//
// is-active/is-enabled exit non-zero for the normal inactive/disabled/not-found
// states, so the exit status alone cannot distinguish them from a real failure.
// Previously BOTH errors were discarded, which laundered an unreachable user bus
// or a missing systemctl into a plausible-looking "installed, not running"
// snapshot derived from the on-disk file (#725). The verdict word is now the
// discriminator: documented states are normal, and anything else is returned as
// an error so `service status` reports "systemctl could not answer" instead of
// inventing a state. The on-disk unit file remains the authoritative installed
// signal for the states systemd does answer.
func (m *systemdManager) status(label string) (serviceState, error) {
	unit := m.unitPath(label)
	state := serviceState{UnitPath: unit}
	if _, err := os.Stat(unit); err == nil {
		state.Installed = true
	} else if !os.IsNotExist(err) {
		return state, fmt.Errorf("stat unit %s: %w", unit, err)
	}

	base := filepath.Base(unit)
	active, err := m.queryState(systemctlIsActive, base)
	if err != nil {
		return state, err
	}
	state.Running = active == "active"

	enabled, err := m.queryState(systemctlIsEnabled, base)
	if err != nil {
		return state, err
	}
	if systemctlEnabledMeansInstalled(enabled) {
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
