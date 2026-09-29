package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countingRemover fails the first failures calls and succeeds after that.
type countingRemover struct {
	failures int
	calls    int
}

// remove is the injected removal.
func (c *countingRemover) remove(string) error {
	c.calls++
	if c.calls <= c.failures {
		return errors.New("The process cannot access the file because it is being used by another process.")
	}
	return nil
}

// TestRemoveAllRetrying_SucceedsAfterTransientFailures pins that a lock that
// clears within the attempt budget does not fail the removal, and that the
// helper sleeps once per retry and not before the first attempt.
func TestRemoveAllRetrying_SucceedsAfterTransientFailures(t *testing.T) {
	r := &countingRemover{failures: 3}
	sleeps := 0
	err := RemoveAllRetrying("x", r.remove, 10, func(time.Duration) { sleeps++ })
	if err != nil {
		t.Fatalf("removal failed: %v", err)
	}
	if r.calls != 4 {
		t.Errorf("remove called %d times, want 4", r.calls)
	}
	if sleeps != 3 {
		t.Errorf("slept %d times, want 3", sleeps)
	}
}

// TestRemoveAllRetrying_FreeDirCostsNoSleep pins the fast path: a directory
// that is free is removed on the first attempt with no sleep at all.
func TestRemoveAllRetrying_FreeDirCostsNoSleep(t *testing.T) {
	r := &countingRemover{}
	sleeps := 0
	if err := RemoveAllRetrying("x", r.remove, 10, func(time.Duration) { sleeps++ }); err != nil {
		t.Fatalf("removal failed: %v", err)
	}
	if r.calls != 1 || sleeps != 0 {
		t.Errorf("calls=%d sleeps=%d, want 1 and 0", r.calls, sleeps)
	}
}

// TestRemoveAllRetrying_GivesUpAfterTheBudget pins that a lock held past the
// last attempt surfaces as an error, so a real leak still fails the test.
func TestRemoveAllRetrying_GivesUpAfterTheBudget(t *testing.T) {
	r := &countingRemover{failures: 100}
	err := RemoveAllRetrying("x", r.remove, 5, func(time.Duration) {})
	if err == nil {
		t.Fatal("removal reported success while every attempt failed")
	}
	if r.calls != 5 {
		t.Errorf("remove called %d times, want 5", r.calls)
	}
}

// TestInsideTempDir pins the guard that keeps ReleaseTempDir from removing a
// directory that is not a temp dir.
func TestInsideTempDir(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		dir  string
		want bool
	}{
		{tmp, true},
		{filepath.Join(tmp, "nested", "deeper"), true},
		{os.TempDir(), true},
		{filepath.Join(os.TempDir(), "..", "not-temp-sibling"), false},
		{filepath.Dir(filepath.Clean(os.TempDir())), false},
	}
	for _, c := range cases {
		if got := insideTempDir(c.dir); got != c.want {
			t.Errorf("insideTempDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}
