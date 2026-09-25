package embedqueue_test

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// storeTempDir returns a temp dir for a sqlite file. It removes the dir itself
// at cleanup instead of t.TempDir. On Windows a closed sqlite handle can hold
// the file for a short time after Close, and t.TempDir then fails the test on
// "The process cannot access the file". The removal retries for up to two
// seconds there; a handle that stays open still fails the test.
func storeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "embedqueue-store-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		err := os.RemoveAll(dir)
		for i := 0; err != nil && runtime.GOOS == "windows" && i < 40; i++ {
			time.Sleep(50 * time.Millisecond)
			err = os.RemoveAll(dir)
		}
		if err != nil {
			t.Errorf("remove temp dir %s: %v", dir, err)
		}
	})
	return dir
}
