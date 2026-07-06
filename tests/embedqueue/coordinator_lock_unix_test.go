//go:build unix

package embedqueue_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
)

// These tests are unix-only: AcquireCoordinatorLock is backed by flock and only
// enforces mutual exclusion under //go:build unix. On other platforms it is a
// no-op that always succeeds, so the ErrCoordinatorLocked assertions below would
// not hold — gate them to the same build tag as the implementation.

// --- C3: single-coordinator lock (issue #435) ---

// TestCoordinatorLock_DetectAndRefuse pins the detect-and-refuse guard: a second
// acquisition of the same lock path is refused with ErrCoordinatorLocked, and the
// path becomes acquirable again after the first holder releases.
func TestCoordinatorLock_DetectAndRefuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embed-coordinator.lock")

	first, err := embedqueue.AcquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := embedqueue.AcquireCoordinatorLock(path); !errors.Is(err, embedqueue.ErrCoordinatorLocked) {
		t.Fatalf("second acquire while held: err = %v, want ErrCoordinatorLocked", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// After release the lock is free again.
	second, err := embedqueue.AcquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

// TestCoordinatorLock_ReleaseIdempotent pins that Release is safe to call twice and
// on a nil lock (defensive shutdown paths).
func TestCoordinatorLock_ReleaseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embed-coordinator.lock")
	l, err := embedqueue.AcquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second release should be a no-op: %v", err)
	}
	var nilLock *embedqueue.CoordinatorLock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil release should be a no-op: %v", err)
	}
}
