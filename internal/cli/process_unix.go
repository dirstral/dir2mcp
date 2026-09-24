//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// processIsAlive reports whether a process with the given pid is currently
// alive and signalable by the calling user. It uses POSIX signal 0, which
// performs the kernel's permission/existence check without delivering a
// signal; ESRCH means "no such process", EPERM means "exists but not
// owned by us" (which we treat as "alive enough to leave alone").
func processIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// Treat EPERM (process exists, signal blocked) as alive so we don't
	// blow away a pid file for a process we shouldn't touch.
	return errors.Is(err, syscall.EPERM)
}

// requestProcessStop asks the process to shut down gracefully with SIGTERM.
// The daemon child and the foreground server both turn SIGTERM into a clean
// shutdown.
func requestProcessStop(proc *os.Process) error {
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("send SIGTERM to %d: %w", proc.Pid, err)
	}
	return nil
}

// forceProcessStop ends the process with SIGKILL. stopDaemon uses it only
// after the graceful window expires.
func forceProcessStop(proc *os.Process) error {
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("send SIGKILL to %d: %w", proc.Pid, err)
	}
	return nil
}

// forceStopName names the last-resort stop in error messages.
const forceStopName = "SIGKILL"
