//go:build unix

package ingest

import (
	"os/exec"
	"syscall"
)

// setRecognizeBackendProcAttr puts the managed recognize backend in its own
// process group, so termination reaches the whole tree — `sh -c` may fork
// rather than exec, and an orphaned grandchild would otherwise survive
// shutdown holding the daemon's log pipe.
func setRecognizeBackendProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalRecognizeBackend delivers sig to the backend's whole process group.
func signalRecognizeBackend(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
