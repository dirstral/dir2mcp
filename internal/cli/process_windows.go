//go:build windows

package cli

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// processIsAlive reports whether a process with the given pid is currently
// alive. Windows has no signal 0, so the check opens the process and waits on
// its handle with a zero timeout. A timeout means the process still runs. An
// access-denied error means the process exists but belongs to another user;
// the check treats that as alive, the same as EPERM on unix.
func processIsAlive(pid int) bool {
	// A pid outside the Windows DWORD range would alias another process after
	// the uint32 conversion below, so it is never alive.
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		// The handle is open, so the process object exists. Keep the pid
		// record rather than remove it on an unclear result.
		return true
	}
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// requestProcessStop ends the process. Windows cannot send SIGTERM to a
// process in another console, so there is no graceful stop: the process ends
// at once through TerminateProcess. The sqlite store uses transactions, so an
// abrupt stop does not corrupt the index.
func requestProcessStop(proc *os.Process) error {
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate process %d: %w", proc.Pid, err)
	}
	return nil
}

// forceProcessStop ends the process through TerminateProcess. On Windows it
// is the same operation as requestProcessStop.
func forceProcessStop(proc *os.Process) error {
	return requestProcessStop(proc)
}

// forceStopName names the last-resort stop in error messages.
const forceStopName = "TerminateProcess"
