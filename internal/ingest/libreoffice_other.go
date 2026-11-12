//go:build !unix

package ingest

import "os/exec"

// reapLibreOfficeGroup is a no-op without process groups: the direct child has
// already exited, and a PID-based kill could hit a reused PID.
func reapLibreOfficeGroup(*exec.Cmd) {}
