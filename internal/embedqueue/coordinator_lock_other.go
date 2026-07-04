//go:build !unix

package embedqueue

// platformLock is empty on platforms without flock; the guard degrades to a no-op.
type platformLock struct{}

// AcquireCoordinatorLock is a no-op on non-unix platforms: it always succeeds so
// the single-binary default keeps working, forgoing the cross-process coordinator
// guard (issue #435 C3) there. dir2mcp ships darwin/linux binaries, where the unix
// flock implementation applies.
func AcquireCoordinatorLock(_ string) (*CoordinatorLock, error) {
	return &CoordinatorLock{}, nil
}

// Release is a no-op on non-unix platforms.
func (l *CoordinatorLock) Release() error { return nil }
