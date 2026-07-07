//go:build !darwin && !linux

package cli

import (
	"fmt"
	"runtime"
)

// newServiceManager reports that login auto-start is not wired for this
// platform. macOS (launchd) and Linux (systemd user units) are supported;
// this stub covers every other GOOS.
func newServiceManager() (serviceManager, error) {
	return nil, fmt.Errorf("dir2mcp service is not supported on %s (macOS/launchd and Linux/systemd only)", runtime.GOOS)
}
