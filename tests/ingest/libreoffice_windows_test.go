//go:build windows

package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestLibreOfficeExtract_WindowsTimeoutKillsTree checks the Windows timeout
// path. On Windows soffice.exe starts soffice.bin as a separate process, so a
// timeout must stop the whole process tree, not only the launcher. The fake
// launcher here starts a long-lived child (ping), records its PID, and waits.
func TestLibreOfficeExtract_WindowsTimeoutKillsTree(t *testing.T) {
	dir := testutil.TempDir(t)
	pidFile := filepath.Join(dir, "child.pid")
	launcher := filepath.Join(dir, "soffice.cmd")
	script := "@echo off\r\n" +
		"powershell -NoProfile -NonInteractive -Command \"$p = Start-Process -FilePath ping -ArgumentList '-n','120','127.0.0.1' -PassThru -WindowStyle Hidden; Set-Content -Path $env:CHILD_PID -Value $p.Id; Wait-Process -Id $p.Id\"\r\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHILD_PID", pidFile)

	_, err := ingest.NewLibreOfficeExtractor("", launcher, "", 8*time.Second).Extract(context.Background(), "slow.doc", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "libreoffice extraction timed out") {
		t.Fatalf("Extract error = %v, want the libreoffice timeout", err)
	}
	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the fake launcher did not record its child PID before the timeout: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("bad child PID %q: %v", raw, convErr)
	}
	deadline := time.Now().Add(10 * time.Second)
	for windowsProcessRunning(t, pid) {
		if time.Now().After(deadline) {
			_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
			t.Fatalf("child process %d survived the timeout; the process tree was not stopped", pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// windowsProcessRunning reports whether pid is listed by tasklist.
func windowsProcessRunning(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		t.Fatalf("tasklist: %v", err)
	}
	return strings.Contains(string(out), "\""+strconv.Itoa(pid)+"\"")
}
