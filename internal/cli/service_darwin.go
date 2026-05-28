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

func (m *launchdManager) backendName() string { return "launchd" }

func (m *launchdManager) plistPath(label string) string {
	return filepath.Join(m.home, "Library", "LaunchAgents", label+".plist")
}

func (m *launchdManager) domainTarget() string {
	return fmt.Sprintf("gui/%d", m.uid)
}

func (m *launchdManager) serviceTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", m.uid, label)
}

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
		// Not loaded into the user's GUI domain. If the plist is on
		// disk it's installed-but-stopped; otherwise fully absent.
		if state.Installed {
			state.Detail = "installed, not loaded"
		} else {
			state.Detail = "not installed"
		}
		return state, nil
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
