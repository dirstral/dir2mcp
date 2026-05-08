//go:build !unix

package cli

import (
	"context"
	"os/exec"
)

// detachFromTerminal is a no-op on non-unix platforms. Daemon mode is
// unsupported on Windows; up.go's daemon launcher checks isDaemonSupported
// before reaching this stub.
func detachFromTerminal(_ *exec.Cmd) {}

// installDaemonChildSignalHandler is a no-op on non-unix platforms —
// see detachFromTerminal. Daemon mode is gated upstream so this stub
// is unreachable in practice.
func installDaemonChildSignalHandler(_ context.CancelFunc) {}

// isDaemonSupported reports whether the current platform supports
// daemonization. Always false on non-unix; up.go falls back to
// foreground mode when this is false.
func isDaemonSupported() bool { return false }
