package tests

import (
	"runtime"
	"testing"
)

// skipOnWindows skips a test that needs a unix-only feature. The reason
// names the feature, so a skipped test is never silent.
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(reason)
	}
}
