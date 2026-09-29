package testutil

import (
	"os"
	"sync"
	"testing"
)

var cwdMu sync.Mutex

// WithWorkingDir runs fn with process cwd switched to dir and restores it.
func WithWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()

	cwdMu.Lock()
	defer cwdMu.Unlock()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	fn()
}
