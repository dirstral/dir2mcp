//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// detachFromTerminal configures the spawned daemon child so that closing
// the parent terminal does not also kill it. Setsid puts the child into
// its own session and process group, making it the session leader; from
// that point the controlling terminal is detached and SIGHUP from the
// terminal close is not delivered.
func detachFromTerminal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// installDaemonChildSignalHandler converts SIGTERM and SIGINT into a
// cancel() call. The daemon child uses this in place of the foreground
// stdin q+Enter listener so `dir2mcp down` (which sends SIGTERM) and a
// stray Ctrl-C both trigger a graceful shutdown via the existing event
// loop's <-runCtx.Done() path.
//
// The signal handler runs for the lifetime of the process; we don't
// bother stopping it on graceful exit because the process is exiting
// anyway.
func installDaemonChildSignalHandler(cancel context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		cancel()
	}()
}

// isDaemonSupported reports whether the current platform supports
// daemonization (fork/setsid). Always true on unix.
func isDaemonSupported() bool { return true }
