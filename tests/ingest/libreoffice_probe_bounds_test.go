package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSofficeProbeBinary_WindowsUsesTheConsoleLauncher: on Windows,
// soffice.exe --version waits for Enter, so the probe runs soffice.com beside
// it when that file exists. Other systems and other names are probed as they
// are.
//
// Mutant killed: probing soffice.exe on Windows, which times out and disables
// a working installation.
func TestSofficeProbeBinary_WindowsUsesTheConsoleLauncher(t *testing.T) {
	t.Parallel()
	dir := testutil.TempDir(t)
	exe := filepath.Join(dir, "soffice.exe")
	com := filepath.Join(dir, "soffice.com")
	for _, p := range []string{exe, com} {
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if got := ingest.SofficeProbeBinaryForTest(exe, "windows"); got != com {
		t.Errorf("windows probe binary = %q, want %q", got, com)
	}
	if got := ingest.SofficeProbeBinaryForTest(exe, "linux"); got != exe {
		t.Errorf("linux probe binary = %q, want %q", got, exe)
	}
	lone := filepath.Join(testutil.TempDir(t), "soffice.exe")
	if got := ingest.SofficeProbeBinaryForTest(lone, "windows"); got != lone {
		t.Errorf("windows probe binary with no soffice.com = %q, want %q", got, lone)
	}
}

// TestLibreOfficeWorkDirProbe_BoundsTheRemoval: a removal that hangs after the
// create call succeeded makes the probe report unavailable within its limit.
//
// Mutant killed: removing the work directory outside the limit, which blocks
// the probe until the filesystem recovers.
func TestLibreOfficeWorkDirProbe_BoundsTheRemoval(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	mk := func(string) (string, error) { return "work", nil }
	rm := func(string) error { <-release; return nil }

	start := time.Now()
	if ok := ingest.LibreOfficeWorkDirUsableWithinForTest("root", 50*time.Millisecond, mk, rm); ok {
		t.Fatal("probe = usable, want unavailable when the removal hangs past the limit")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("probe took %v, want it to return at its limit", took)
	}
	if ok := ingest.LibreOfficeWorkDirUsableWithinForTest("root", time.Second, mk, func(string) error { return nil }); !ok {
		t.Fatal("probe = unavailable, want usable when create and removal succeed")
	}
}
