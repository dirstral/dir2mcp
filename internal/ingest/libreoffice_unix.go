//go:build unix

package ingest

import (
	"os/exec"
	"syscall"
)

// prepareLibreOfficeCmd makes cmd stoppable as a whole tree. soffice is a
// launcher that starts soffice.bin, so the command runs in its own process
// group and a cancelled context (a timeout) sends SIGKILL to the whole group,
// helpers included.
func prepareLibreOfficeCmd(cmd *exec.Cmd) {
	setRecognizeBackendProcAttr(cmd)
	cmd.Cancel = func() error { return signalRecognizeBackend(cmd.Process.Pid, syscall.SIGKILL) }
}

// reapLibreOfficeGroup kills whatever is left of a finished LibreOffice
// command's process group. A helper can outlive the launcher; the command ran
// in its own group (prepareLibreOfficeCmd), so one signal to the group removes
// every leftover. A group that is already gone returns ESRCH, which is
// ignored. A command that never started has no Process and is skipped.
func reapLibreOfficeGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
