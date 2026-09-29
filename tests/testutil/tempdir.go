package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// Windows lets a closed sqlite file stay "in use by another process" for a
// while after the last handle is gone (an antivirus scan, the search indexer,
// or the kernel's own deferred close). t.TempDir removes the directory once,
// with no retry, so a test that ran `up` fails at cleanup with "The process
// cannot access the file because it is being used by another process" although
// the test itself passed. The removal below retries for up to 30 s. A lock
// that outlasts the budget is logged and the directory is left behind: the
// holder is outside the process (lsof and a goroutine dump show no handle and
// no dir2mcp goroutine once the command returns, on the up, reindex and
// exit-3 paths), the runner is discarded after the job, and a passed test
// must not turn red on a file the test does not own any more.
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

// ReleaseTempDir registers a cleanup that removes dir with a retry on Windows
// and logs, without a failure, a directory that stays locked past the budget.
// Cleanups run last-in first-out, so a call made right after the directory is
// created runs after every cleanup the test registers later (a store close, a
// server stop). Call it at creation time and nowhere else: a call from a
// helper the test enters later runs before the cleanups registered in between,
// and then it waits on a handle that a later cleanup would have closed.
// It does nothing on other platforms, and nothing for a dir outside the
// process temp directory: only a temp dir may be removed from here.
func ReleaseTempDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "windows" || !insideTempDir(dir) {
		return
	}
	t.Cleanup(func() {
		if err := RemoveAllRetrying(dir, os.RemoveAll, releaseAttempts, time.Sleep); err != nil {
			t.Logf("temp dir %s stays locked after %d attempts and is left behind: %v", dir, releaseAttempts, err)
		}
	})
}

// TempDir returns a per-test temp directory that a locked file cannot turn
// into a failure. On Windows it is a plain os.MkdirTemp under the process temp
// directory with the release from ReleaseTempDir, and not t.TempDir: the
// framework's own cleanup would still fail the test on the same lock. On other
// platforms it is t.TempDir. The tests/cli package uses it for every temp dir,
// so a test cannot pick the wrong one.
func TempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("", tempDirPrefix(t.Name()))
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	ReleaseTempDir(t, dir)
	return dir
}

// tempDirPrefix turns a test name into a directory-name prefix the way
// t.TempDir does: every path separator and every character that is not safe
// in a file name becomes an underscore, so a subtest name with a slash or a
// quote cannot fail MkdirTemp.
func tempDirPrefix(name string) string {
	mapper := func(r rune) rune {
		if r < utf8.RuneSelf {
			const allowed = "!#$%&()+,-.=@^_{}~ "
			if '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' {
				return r
			}
			if strings.ContainsRune(allowed, r) {
				return r
			}
		} else if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return '_'
	}
	return strings.Map(mapper, name)
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
