//go:build !unix

package ingest

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// taskkillTimeout bounds the `taskkill /T /F` call in the cancel callback. os/exec
// starts the WaitDelay timer only after Cancel returns, so a hung taskkill would
// otherwise keep the timed-out conversion alive. When it expires, the direct
// child is killed as the fallback.
const taskkillTimeout = 10 * time.Second

// prepareLibreOfficeCmd makes cmd stoppable as a whole tree. On Windows the
// launcher soffice.exe starts soffice.bin as a separate process, and killing
// only the launcher leaves soffice.bin running. A cancelled context (a
// timeout) therefore runs `taskkill /T /F`, which stops the launcher and every
// process it started. If taskkill is unavailable, fails, or does not finish
// within taskkillTimeout, the direct child is killed as a fallback.
func prepareLibreOfficeCmd(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
			defer cancel()
			tk := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
			tk.WaitDelay = time.Second
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
