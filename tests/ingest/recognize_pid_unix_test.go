//go:build unix

package tests

import "syscall"

// pidAlive reports whether pid names a live process. Signal 0 checks for the
// process and delivers nothing.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
