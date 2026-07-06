//go:build darwin

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartToken returns an opaque token that identifies the specific
// running instance behind pid — the kernel-recorded process start time.
// Combined with the pid it distinguishes a live daemon from an unrelated
// process the OS assigned the same (recycled) pid after a crash without
// cleanup (issue #418). Returns ok=false when the process is gone or its
// start time can't be read, in which case callers fall back to a bare
// liveness check (i.e. pre-#418 behaviour, no pid-reuse protection).
func processStartToken(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return "", false
	}
	st := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", int64(st.Sec), int64(st.Usec)), true
}
