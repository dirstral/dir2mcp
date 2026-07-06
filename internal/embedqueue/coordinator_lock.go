package embedqueue

import "errors"

// ErrCoordinatorLocked is returned by AcquireCoordinatorLock when another live
// process already holds the corpus's coordinator lock. The caller MUST refuse to
// start a second coordinator for the corpus: two coordinators on one corpus each
// enqueue the same pending chunks and (with the default in-process MemBroker,
// which cannot dedup across processes) double-embed them (issue #435 C3).
var ErrCoordinatorLocked = errors.New("embedqueue: coordinator already running for this corpus")

// CoordinatorLock is a single-holder, cross-process advisory lock guarding a
// corpus's embedding coordinator (issue #435 C3, "detect + refuse"). It is a guard
// against the accidental double-`up` / restart-race case, NOT a full multi-node
// leader election (that lands with the standalone-worker deployment, #249): it
// only ensures at most one coordinator runs per corpus on a single host.
//
// The default broker is in-process (MemBroker), so two daemons against the same
// corpus each get a PRIVATE queue and cannot coordinate through the broker at all;
// only a host-level lock (this) or the shared SQLite broker can catch that. The
// lock is taken on a file in the corpus state dir, so it also naturally releases
// if the holder process dies (the OS drops the advisory lock).
//
// The concrete lock is OS-specific; see coordinator_lock_unix.go and
// coordinator_lock_other.go.
type CoordinatorLock struct {
	platformLock
}
