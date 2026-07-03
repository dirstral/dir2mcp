//go:build unix

package embedqueue

import (
	"fmt"
	"os"
	"syscall"
)

// platformLock holds the open lock file whose flock we own. Closing the file (or
// the process exiting) releases the flock.
type platformLock struct {
	f *os.File
}

// AcquireCoordinatorLock takes an exclusive, non-blocking advisory (flock) lock on
// lockPath, creating the file if absent. It returns ErrCoordinatorLocked when
// another process already holds it — the caller must then refuse to start a
// coordinator for this corpus (issue #435 C3). The returned lock is released by
// Release (or when the process exits and the OS drops the flock).
func AcquireCoordinatorLock(lockPath string) (*CoordinatorLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("embedqueue: open coordinator lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrCoordinatorLocked
		}
		return nil, fmt.Errorf("embedqueue: lock coordinator lock: %w", err)
	}
	return &CoordinatorLock{platformLock{f: f}}, nil
}

// Release drops the advisory lock and closes the lock file. It is safe to call
// once; a nil or already-released lock is a no-op.
func (l *CoordinatorLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// Unlock explicitly; closing the fd would release it too, but being explicit
	// keeps the intent clear and drops the lock even if Close is deferred.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
