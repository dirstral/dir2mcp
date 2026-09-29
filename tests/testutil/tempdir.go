package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Windows lets a closed sqlite file stay "in use by another process" for a
// while after the last handle is gone (an antivirus scan, the search indexer,
// or the kernel's own deferred close). t.TempDir removes the directory once,
// with no retry, so a test that ran `up` fails at cleanup with "The process
// cannot access the file because it is being used by another process" although
// the test itself passed. The removal below retries for up to 30 s; a lock
// held longer than that still fails the test.
const (
	releaseAttempts = 300
	releaseInterval = 100 * time.Millisecond
)

// RemoveAllRetrying removes dir with remove and retries a failure up to
// attempts times, with sleep between attempts. It returns the last error. The
// first attempt never sleeps, so a directory that is free costs nothing extra.
func RemoveAllRetrying(dir string, remove func(string) error, attempts int, sleep func(time.Duration)) error {
	err := remove(dir)
	for i := 1; err != nil && i < attempts; i++ {
		sleep(releaseInterval)
		err = remove(dir)
	}
	return err
}

// ReleaseTempDir registers a cleanup that removes dir with a retry on Windows.
// Cleanups run last-in first-out, so a call made after t.TempDir runs before
// the test framework's own single-shot removal, which then finds nothing left.
// It does nothing on other platforms, and nothing for a dir outside the
// process temp directory: only a temp dir may be removed from here.
func ReleaseTempDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "windows" || !insideTempDir(dir) {
		return
	}
	t.Cleanup(func() {
		if err := RemoveAllRetrying(dir, os.RemoveAll, releaseAttempts, time.Sleep); err != nil {
			t.Errorf("remove temp dir %s: %v", dir, err)
		}
	})
}

// TempDir returns t.TempDir() with the Windows release registered on it. Use
// it for a directory that `up` runs in without WithWorkingDir.
func TempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ReleaseTempDir(t, dir)
	return dir
}

// insideTempDir reports whether dir is the process temp directory or below
// it. The comparison is case-insensitive, because Windows paths are.
func insideTempDir(dir string) bool {
	base, err := filepath.Abs(os.TempDir())
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(strings.ToLower(base), strings.ToLower(abs))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
