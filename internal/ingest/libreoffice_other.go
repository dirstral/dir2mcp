//go:build !unix

package ingest

import (
	"os/exec"
	"runtime"
	"strconv"
)

// prepareLibreOfficeCmd makes cmd stoppable as a whole tree. On Windows the
// launcher soffice.exe starts soffice.bin as a separate process, and killing
// only the launcher leaves soffice.bin running. A cancelled context (a
// timeout) therefore runs `taskkill /T /F`, which stops the launcher and every
// process it started. If taskkill is unavailable or fails, the direct child is
// killed as a fallback.
func prepareLibreOfficeCmd(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
			if err := tk.Run(); err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}

// reapLibreOfficeGroup is a no-op without process groups: after a normal exit
// the launcher is gone, so its tree cannot be found by its PID any more, and a
// PID-based kill could hit a reused PID.
func reapLibreOfficeGroup(*exec.Cmd) {}
