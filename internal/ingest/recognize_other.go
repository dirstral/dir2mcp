//go:build !unix

package ingest

import (
	"os"
	"os/exec"
	"syscall"
)

// Non-unix stubs: no process groups; best-effort kill of the direct child.

func setRecognizeBackendProcAttr(*exec.Cmd) {}

func signalRecognizeBackend(pid int, _ syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
