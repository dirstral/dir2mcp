//go:build !darwin

package cli

import (
	"fmt"
	"runtime"
)

// newServiceManager reports that login auto-start is not yet wired for
// this platform. macOS (launchd) is supported today; a Linux systemd
// user-unit backend is a planned follow-up.
func newServiceManager() (serviceManager, error) {
	return nil, fmt.Errorf("dir2mcp service is not yet supported on %s (macOS/launchd only for now)", runtime.GOOS)
}
