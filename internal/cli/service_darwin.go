//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchdManager supervises a per-corpus dir2mcp daemon as a macOS
// LaunchAgent. runCmd is injectable so the bootstrap/bootout/print
// sequence can be unit-tested without touching the real launchd.
type launchdManager struct {
	uid    int
	home   string
	runCmd func(name string, args ...string) (string, error)
}

// newServiceManager returns the macOS launchd backend.
func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return &launchdManager{
		uid:  os.Getuid(),
		home: home,
		runCmd: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
	}, nil
}

// backendName identifies this manager in CLI output.
func (m *launchdManager) backendName() string { return "launchd" }

// plistPath returns the canonical LaunchAgent plist path for label.
// filepath.Base clamps any residual path separators in label so it
// cannot escape the LaunchAgents directory, even if upstream validation
// is somehow bypassed.
func (m *launchdManager) plistPath(label string) string {
	return filepath.Join(m.home, "Library", "LaunchAgents", filepath.Base(label)+".plist")
}

// domainTarget returns the launchctl GUI domain target for this user.
func (m *launchdManager) domainTarget() string {
	return fmt.Sprintf("gui/%d", m.uid)
}

// serviceTarget returns the launchctl service target for label in this
// user's GUI domain.
func (m *launchdManager) serviceTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", m.uid, label)
}

// install writes the LaunchAgent plist and bootstraps it into the user's GUI
// domain, kicking off an immediate (re)start via kickstart -k.
//
// It is transactional as far as launchd permits (#724). The prior plist and
// whether the prior service was loaded are captured first; the replacement is
// written atomically; and any supervisor step that fails rolls the machine back
// to that captured state — the previous definition on disk, and, when we had
// unloaded it, the previous service booted back in. A first-time install whose
// supervisor step fails leaves no plist behind at all, so it cannot silently
// activate at the next login.
//
// What remains non-atomic: launchd itself has no transaction, so between the
// bootout and the successful re-bootstrap of the previous service there is a
// window in which the old daemon is down. Rollback closes that window rather
// than preventing it, and reports explicitly when it could not.
func (m *launchdManager) install(spec serviceSpec) (string, error) {
	plistPath := m.plistPath(spec.Label)
	// launchd must be able to read this; it is a service definition, not
	// corpus-derived content (#726).
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	txn, err := beginUnitTxn(plistPath, 0o644)
	if err != nil {
		return "", fmt.Errorf("read existing plist %s: %w", plistPath, err)
	}
	priorLoaded := txn.had && m.serviceLoaded(spec.Label)
	if err := txn.write(renderLaunchdPlist(spec)); err != nil {
		return plistPath, fmt.Errorf("write plist %s: %w", plistPath, err)
	}
	// Reload idempotently: bootout an existing copy — but only a positively
	// identified "nothing loaded" result is benign (#723).
	if out, err := m.runCmd("launchctl", "bootout", m.domainTarget(), plistPath); err != nil && !launchctlBootoutReportsAbsent(out) {
		// Nothing was unloaded, so the previous service is still live on its
		// previous in-memory definition: restoring the file is a complete undo.
		return plistPath, m.rollbackInstall(txn, spec.Label, false, false,
			fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(out)))
	}
	if out, err := m.runCmd("launchctl", "bootstrap", m.domainTarget(), plistPath); err != nil {
		return plistPath, m.rollbackInstall(txn, spec.Label, true, priorLoaded,
			fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(out)))
	}
	// RunAtLoad starts it on bootstrap; kickstart -k forces a clean
	// (re)start so reinstalls pick up the new plist immediately.
	if out, err := m.runCmd("launchctl", "kickstart", "-k", m.serviceTarget(spec.Label)); err != nil {
		return plistPath, m.rollbackInstall(txn, spec.Label, true, priorLoaded,
			fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(out)))
	}
	return plistPath, nil
}

// serviceLoaded reports whether label is currently registered in the user's GUI
// domain. A launchctl failure counts as "not loaded": rollback then leaves the
// service down instead of bootstrapping a definition launchd may already hold.
func (m *launchdManager) serviceLoaded(label string) bool {
	_, err := m.runCmd("launchctl", "print", m.serviceTarget(label))
	return err == nil
}

// rollbackInstall undoes a partially applied install and returns cause annotated
// with the resulting state.
//
// unloadNew bootouts a replacement that may already be registered; reloadPrior
// bootstraps the restored definition because the service it replaced was loaded
// before the install started.
func (m *launchdManager) rollbackInstall(txn *unitTxn, label string, unloadNew, reloadPrior bool, cause error) error {
	var problems []string
	if unloadNew {
		if out, err := m.runCmd("launchctl", "bootout", m.domainTarget(), txn.path); err != nil && !launchctlBootoutReportsAbsent(out) {
			problems = append(problems, fmt.Sprintf("could not boot out the replacement service %s: %v: %s",
				label, err, strings.TrimSpace(out)))
		}
	}
	if err := txn.rollback(); err != nil {
		problems = append(problems, fmt.Sprintf("could not restore %s: %v", txn.path, err))
		return wrapInstallRollback(cause, txn, problems)
	}
	if reloadPrior {
		if out, err := m.runCmd("launchctl", "bootstrap", m.domainTarget(), txn.path); err != nil {
			problems = append(problems, fmt.Sprintf("the previous service %s is no longer loaded: %v: %s",
				label, err, strings.TrimSpace(out)))
		}
	}
	return wrapInstallRollback(cause, txn, problems)
}

// uninstall bootouts the service from the user's GUI domain and removes the
// plist file. Returns removed=false when the service was not installed.
//
// Only a positively identified "already unloaded" bootout result is treated as
// success (#723). Any other launchctl failure keeps the plist on disk and is
// returned: deleting the definition of a daemon that is still running would
// orphan it and destroy the file needed to retry.
func (m *launchdManager) uninstall(label string) (string, bool, error) {
	plistPath := m.plistPath(label)
	// Unload first, then remove the plist so it won't reload at next login.
	if out, err := m.runCmd("launchctl", "bootout", m.domainTarget(), plistPath); err != nil && !launchctlBootoutReportsAbsent(out) {
		return plistPath, false, fmt.Errorf(
			"launchctl bootout %s: %w: %s (the service may still be running; %s was kept so you can retry)",
			m.serviceTarget(label), err, strings.TrimSpace(out), plistPath)
	}
	if _, err := os.Stat(plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return plistPath, false, nil
		}
		return plistPath, false, fmt.Errorf("stat plist %s: %w", plistPath, err)
	}
	if err := os.Remove(plistPath); err != nil {
		return plistPath, false, fmt.Errorf("remove plist %s: %w", plistPath, err)
	}
	return plistPath, true, nil
}

// status queries the launchd GUI domain for label and returns whether the
// service is installed (plist on disk) and running (active in domain).
// Only explicit "service not found" outputs from launchctl are treated
// as absent state; other command failures are returned as errors.
func (m *launchdManager) status(label string) (serviceState, error) {
	plistPath := m.plistPath(label)
	state := serviceState{UnitPath: plistPath}
	if _, err := os.Stat(plistPath); err == nil {
		state.Installed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, fmt.Errorf("stat plist %s: %w", plistPath, err)
	}

	out, err := m.runCmd("launchctl", "print", m.serviceTarget(label))
	if err != nil {
		// Distinguish "service not registered" (normal absent state) from
		// real command failures (permission denied, domain error, etc.).
		if launchctlPrintReportsAbsent(out) {
			if state.Installed {
				state.Detail = "installed, not loaded"
			} else {
				state.Detail = "not installed"
			}
			return state, nil
		}
		return state, fmt.Errorf("launchctl print %s: %w: %s", m.serviceTarget(label), err, strings.TrimSpace(out))
	}
	state.Installed = true
	if strings.Contains(out, "state = running") {
		state.Running = true
		state.Detail = "running"
	} else {
		state.Detail = "loaded, not running"
	}
	return state, nil
}
