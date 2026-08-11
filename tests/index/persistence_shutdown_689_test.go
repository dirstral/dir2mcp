package tests

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #689: the periodic autosave called every backend with
// context.Background(), so shutdown could not reach a save that had already
// started. StopAndSave then returned on its own deadline while that save still
// owned the index, and the CLI closed the index underneath the writer. The
// timeout bounded the wait, not the work.

// blockingSaveIndex blocks inside Save until the test releases it. It records
// whether the save observed a cancelled context, and whether Close was called
// while the save was still running.
type blockingSaveIndex struct {
	coreIndexStub

	// honorCancel selects the backend behaviour. A cooperative backend stops
	// when its context is cancelled. An uncooperative one runs to the release
	// whatever the context says; that is the case the close fence exists for.
	honorCancel bool

	started chan struct{}
	release chan struct{}

	saving        atomic.Bool
	sawCancel     atomic.Bool
	closedDuring  atomic.Bool
	closeAttempts atomic.Int32
}

func newBlockingSaveIndex(honorCancel bool) *blockingSaveIndex {
	return &blockingSaveIndex{
		honorCancel: honorCancel,
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
}

func (i *blockingSaveIndex) Save(ctx context.Context, _ string) error {
	i.saving.Store(true)
	defer i.saving.Store(false)

	select {
	case i.started <- struct{}{}:
	default:
	}

	if i.honorCancel {
		select {
		case <-i.release:
		case <-ctx.Done():
			i.sawCancel.Store(true)
			return ctx.Err()
		}
		return nil
	}

	<-i.release
	if ctx.Err() != nil {
		i.sawCancel.Store(true)
	}
	return nil
}

func (i *blockingSaveIndex) Load(context.Context, string) error { return nil }

// Close stands in for the CLI teardown that must never overlap a save.
func (i *blockingSaveIndex) Close() error {
	i.closeAttempts.Add(1)
	if i.saving.Load() {
		i.closedDuring.Store(true)
	}
	return nil
}

// waitForSaveStart blocks until the index reports that a save began.
func waitForSaveStart(t *testing.T, ix *blockingSaveIndex) {
	t.Helper()
	select {
	case <-ix.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic autosave never started")
	}
}

// TestAutosaveObservesShutdownCancellation proves a save that is already
// running sees the shutdown.
//
// Before the fix the tick handed the backend context.Background(), so a save
// could not learn that the manager had been told to stop. It ran to completion
// against an index the caller was already tearing down. A cancelable save can
// instead abandon its temporary file, which leaves the previous good snapshot
// on disk.
func TestAutosaveObservesShutdownCancellation(t *testing.T) {
	ix := newBlockingSaveIndex(true)
	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: ix},
	}, 10*time.Millisecond, nil)

	pm.Start(context.Background())
	waitForSaveStart(t, ix)

	// Stop the manager while the save is in flight. The deadline is short: the
	// final forced save that follows blocks on the same release, and this test
	// only asks whether the periodic save learned about the shutdown.
	stopCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stopErr := pm.StopAndSave(stopCtx)

	// Release the save unconditionally so the test cannot hang if the
	// cancellation never arrives.
	close(ix.release)

	deadline := time.Now().Add(2 * time.Second)
	for ix.saving.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if !ix.sawCancel.Load() {
		t.Fatalf("the in-progress autosave never observed the shutdown cancellation (StopAndSave returned %v)", stopErr)
	}
}

// TestStopAndSaveFencesCloseWhileSaveInFlight proves the ownership contract: if
// the wait runs out while a save still uses the index, StopAndSave says so, and
// the caller must not close the index.
//
// The bounded wait is deliberate. An unbounded one turns a clean stop into a
// hung process, which the supervisor ends with SIGKILL. But the caller cannot
// treat "the wait expired" as "the index is mine again": the save still owns
// the file. ErrSaveInFlight is the difference between the two.
func TestStopAndSaveFencesCloseWhileSaveInFlight(t *testing.T) {
	// The save ignores cancellation, which is the worst case the fence exists
	// for: a backend that cannot be interrupted.
	ix := newBlockingSaveIndex(false)
	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: ix},
	}, 10*time.Millisecond, nil)

	pm.Start(context.Background())
	waitForSaveStart(t, ix)

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	stopErr := pm.StopAndSave(stopCtx)
	elapsed := time.Since(start)

	if !errors.Is(stopErr, index.ErrSaveInFlight) {
		t.Fatalf("StopAndSave returned %v; it must report ErrSaveInFlight so the caller keeps the index open", stopErr)
	}
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("StopAndSave returned %v; it must also report why it stopped waiting", stopErr)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("StopAndSave waited %s; the wait must stay bounded", elapsed)
	}
	if !ix.saving.Load() {
		t.Fatal("the test did not reproduce an in-flight save; nothing was fenced")
	}
	if ix.closeAttempts.Load() != 0 {
		t.Fatal("the persistence manager closed the index itself; only the caller may close it")
	}

	close(ix.release)
	deadline := time.Now().Add(2 * time.Second)
	for ix.saving.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ix.closedDuring.Load() {
		t.Fatal("the index was closed while a save was still writing it")
	}
}

// TestStopAndSaveRunsFinalSaveWhenQuiescent pins the good path: when no periodic
// save is in flight, StopAndSave still writes the final snapshot and reports the
// index as safe to close.
func TestStopAndSaveRunsFinalSaveWhenQuiescent(t *testing.T) {
	ix := &fakePersistIndex{}
	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: ix},
	}, time.Hour, nil)

	pm.Start(context.Background())

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pm.StopAndSave(stopCtx); err != nil {
		t.Fatalf("StopAndSave on an idle manager: %v", err)
	}
	if got := atomic.LoadInt32(&ix.saveCalls); got != 1 {
		t.Fatalf("final forced save ran %d times, want 1", got)
	}
}

// TestAutosaveTickSkipsBackendsAfterShutdown proves a tick that is part way
// through the index list stops starting new backend saves once shutdown begins.
// The tick holds one lock across every index, so without the check the second
// index would start a fresh save after the manager was told to stop.
func TestAutosaveTickSkipsBackendsAfterShutdown(t *testing.T) {
	first := newBlockingSaveIndex(false)
	second := &fakePersistIndex{}

	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: first},
		{Path: "code.idx", Index: second},
	}, 10*time.Millisecond, nil)

	runCtx, cancelRun := context.WithCancel(context.Background())
	pm.Start(runCtx)
	waitForSaveStart(t, first)

	// Cancel while the first index is saving, then let it finish.
	cancelRun()
	close(first.release)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The manager is already cancelled, so this only drains and runs the final
	// forced save, which is allowed to touch every index.
	_ = pm.StopAndSave(stopCtx)

	// The forced final save is the only Save the second index may have seen
	// from this run; the cancelled tick must not have started one.
	if got := atomic.LoadInt32(&second.saveCalls); got > 1 {
		t.Fatalf("the second index was saved %d times; a cancelled tick started a new backend save", got)
	}
}

// blockingSaveIndex must satisfy the interfaces the manager type-asserts.
var (
	_ model.Index       = (*blockingSaveIndex)(nil)
	_ model.Persistable = (*blockingSaveIndex)(nil)
)
