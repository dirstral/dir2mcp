//go:build windows

package cli

import (
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

// processStartToken returns an opaque token that identifies the specific
// running instance behind pid: the process creation time that Windows records.
// Windows reuses pids quickly, so the token lets down, status and up tell our
// live server apart from an unrelated process with a recycled pid (issue
// #418). It reports ok=false when the process is gone or its times cannot be
// read. Callers then fall back to a bare liveness check.
func processStartToken(pid int) (string, bool) {
	// A pid outside the DWORD range would alias another process after the
	// uint32 conversion below.
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false
	}
	return fmt.Sprintf("win.%d", created.Nanoseconds()), true
}
