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

// install writes the LaunchAgent plist and bootstraps it into the user's
// GUI domain, kicking off an immediate (re)start via kickstart -k.
func (m *launchdManager) install(spec serviceSpec) (string, error) {
	plistPath := m.plistPath(spec.Label)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(renderLaunchdPlist(spec)), 0o644); err != nil {
		return "", fmt.Errorf("write plist %s: %w", plistPath, err)
	}
	// Reload idempotently: bootout an existing copy (ignore the error
	// when nothing is loaded yet), then bootstrap the fresh plist.
	_, _ = m.runCmd("launchctl", "bootout", m.domainTarget(), plistPath)
	if out, err := m.runCmd("launchctl", "bootstrap", m.domainTarget(), plistPath); err != nil {
		return plistPath, fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(out))
	}
	// RunAtLoad starts it on bootstrap; kickstart -k forces a clean
	// (re)start so reinstalls pick up the new plist immediately.
	if out, err := m.runCmd("launchctl", "kickstart", "-k", m.serviceTarget(spec.Label)); err != nil {
		return plistPath, fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(out))
	}
	return plistPath, nil
}

// uninstall bootouts the service from the user's GUI domain and removes
// the plist file. Returns removed=false when the service was not installed.
func (m *launchdManager) uninstall(label string) (string, bool, error) {
	plistPath := m.plistPath(label)
	// Unload first (ignore error: it may already be stopped/absent),
	// then remove the plist so it won't reload at next login.
	_, _ = m.runCmd("launchctl", "bootout", m.domainTarget(), plistPath)
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

// launchdNotFoundPhrases are substrings launchctl prints when a service
// is not registered in the domain. Only these known-absent outputs are
// treated as "not installed / not loaded"; any other error is returned
// to the caller so genuine command failures don't masquerade as absent.
var launchdNotFoundPhrases = []string{
	"could not find service",
	"not found in domain",
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
		msg := strings.ToLower(strings.TrimSpace(out))
		for _, phrase := range launchdNotFoundPhrases {
			if strings.Contains(msg, phrase) {
				if state.Installed {
					state.Detail = "installed, not loaded"
				} else {
					state.Detail = "not installed"
				}
				return state, nil
			}
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
