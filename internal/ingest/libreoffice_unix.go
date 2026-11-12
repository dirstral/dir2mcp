//go:build unix

package ingest

import (
	"os/exec"
	"syscall"
)

// reapLibreOfficeGroup kills whatever is left of a finished LibreOffice
// command's process group. soffice is a launcher that starts soffice.bin, and a
// helper can outlive the launcher; the command ran in its own group
// (setRecognizeBackendProcAttr), so one signal to the group removes every
// leftover. A group that is already gone returns ESRCH, which is ignored.
func reapLibreOfficeGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
