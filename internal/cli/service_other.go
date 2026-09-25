//go:build !darwin && !linux

package cli

import (
	"errors"
	"runtime"
)

// newServiceManager reports that login auto-start is not wired for this
// platform. macOS (launchd) and Linux (systemd user units) are supported;
// this stub covers every other GOOS. The message text comes from
// serviceUnsupportedMessage in service.go.
func newServiceManager() (serviceManager, error) {
	return nil, errors.New(serviceUnsupportedMessage(runtime.GOOS))
}
