package tests

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dir2mcp/internal/index"
)

type fakePersistIndex struct {
	loadCalls int32
	saveCalls int32
	loadErr   error
	saveErr   error
	// saveNotify is an optional channel that, when non-nil, will be
	// signalled each time Save is called. Tests can use this to wait
	// deterministically for auto-save ticks instead of relying on sleeps.
	saveNotify chan struct{}
}

// notifyIndex is a minimal implementation of model.Index used by tests
// that need to observe when Load begins. It sends a notification on the
// provided channel immediately when Load is called; if the `done` channel
// is non-nil, Load blocks until it is closed. This allows tests to control
// the duration of the call.
type notifyIndex struct {
	started chan struct{}
	done    chan struct{}
}

func (n *notifyIndex) Add(label uint64, vector []float32) error { return nil }
func (n *notifyIndex) Search(vector []float32, k int) ([]uint64, []float32, error) {
	return nil, nil, nil
}
func (n *notifyIndex) Save(path string) error { return nil }
func (n *notifyIndex) Load(path string) error {
	select {
	case n.started <- struct{}{}:
	default:
	}
	if n.done != nil {
		<-n.done
	}
	return nil
}
func (n *notifyIndex) Close() error { return nil }

func (f *fakePersistIndex) Add(label uint64, vector []float32) error {
	_ = label
	_ = vector
	return nil
}

func (f *fakePersistIndex) Search(vector []float32, k int) ([]uint64, []float32, error) {
	_ = vector
	_ = k
	return nil, nil, nil
}

func (f *fakePersistIndex) Save(path string) error {
	_ = path
	atomic.AddInt32(&f.saveCalls, 1)
	if f.saveNotify != nil {
		// non-blocking send so we don't deadlock if nobody is listening
		select {
		case f.saveNotify <- struct{}{}:
		default:
		}
	}
	return f.saveErr
}

func (f *fakePersistIndex) Load(path string) error {
	_ = path
	atomic.AddInt32(&f.loadCalls, 1)
	return f.loadErr
}

func (f *fakePersistIndex) Close() error { return nil }

func TestPersistenceManager_LoadAndSaveAll(t *testing.T) {
	i1 := &fakePersistIndex{}
	i2 := &fakePersistIndex{}
	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: i1},
		{Path: "code.idx", Index: i2},
	}, time.Second, nil)

	if err := pm.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if err := pm.SaveAll(); err != nil {
		t.Fatalf("SaveAll failed: %v", err)
	}

	if atomic.LoadInt32(&i1.loadCalls) != 1 || atomic.LoadInt32(&i2.loadCalls) != 1 {
		t.Fatalf(
			"unexpected load calls: i1=%d i2=%d",
			atomic.LoadInt32(&i1.loadCalls),
			atomic.LoadInt32(&i2.loadCalls),
		)
	}
	if atomic.LoadInt32(&i1.saveCalls) != 1 || atomic.LoadInt32(&i2.saveCalls) != 1 {
		t.Fatalf(
			"unexpected save calls: i1=%d i2=%d",
			atomic.LoadInt32(&i1.saveCalls),
			atomic.LoadInt32(&i2.saveCalls),
		)
	}
}

func TestPersistenceManager_AutoSaveAndStop(t *testing.T) {
	// use a buffered channel so the timer goroutine won't block if we'd
	// already received the notification
	saveCh := make(chan struct{}, 2)
	i1 := &fakePersistIndex{saveNotify: saveCh}
	pm := index.NewPersistenceManager([]index.IndexedFile{
		{Path: "text.idx", Index: i1},
	}, 20*time.Millisecond, nil)

	pm.Start(context.Background())
	// wait for the first auto-save tick; fail fast if it doesn't happen
	select {
	case <-saveCh:
	// got it!
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout waiting for auto-save tick")
	}

	if err := pm.StopAndSave(context.Background()); err != nil {
		t.Fatalf("StopAndSave failed: %v", err)
	}
	// the final save during StopAndSave should also signal
	select {
	case <-saveCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout waiting for final save")
	}

	if atomic.LoadInt32(&i1.saveCalls) < 2 {
		t.Fatalf("expected at least 2 save calls (tick + final), got %d", atomic.LoadInt32(&i1.saveCalls))
	}
}

// TestPersistenceManager_StartStop_NoWaitGroupRace exercises immediate
// start/stop cycles. This used to be flaky when Start accounted for the
// autosave goroutine after releasing stateMu, because StopAndSave could
// observe a zero wait-group count and return before the goroutine was
// tracked. With the fix in place, this loop is deterministic and stable.
func TestPersistenceManager_StartStop_NoWaitGroupRace(t *testing.T) {
	pm := index.NewPersistenceManager([]index.IndexedFile{{Path: "", Index: &fakePersistIndex{}}}, time.Hour, nil)

	for i := 0; i < 500; i++ {
		pm.Start(context.Background())

		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err := pm.StopAndSave(ctx)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: StopAndSave failed: %v", i, err)
		}
	}
}

// The following tests verify that LoadAll respects the provided context by
// checking both pre-call and inter-iteration cancellation.
func TestPersistenceManager_LoadAll_CancelBefore(t *testing.T) {
	// cancel the context immediately; Load should not be invoked at all.
	i := &fakePersistIndex{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pm := index.NewPersistenceManager([]index.IndexedFile{{Path: "text.idx", Index: i}}, time.Second, nil)
	if err := pm.LoadAll(ctx); err == nil {
		t.Fatalf("expected error from cancelled context, got nil")
	}
	if atomic.LoadInt32(&i.loadCalls) != 0 {
		t.Fatalf("load was called despite cancellation")
	}
}

func TestPersistenceManager_LoadAll_CancelDuring(t *testing.T) {
	// Use an index whose Load notifies when it begins then blocks until
	// we allow it to proceed. We'll cancel the context while it's
	// blocked so that the manager's post-call check should catch the
	// cancellation and return an error before attempting the second index.

	first := &notifyIndex{
		started: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	second := &fakePersistIndex{}
	ctx, cancel := context.WithCancel(context.Background())
	pm := index.NewPersistenceManager(
		[]index.IndexedFile{
			{Path: "first.idx", Index: first},
			{Path: "second.idx", Index: second},
		},
		time.Second,
		nil,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- pm.LoadAll(ctx)
	}()

	// wait for the first load to start, then cancel
	select {
	case <-first.started:
		// good, proceed
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first load to start")
	}
	cancel()
	// signal the first load to finish now that we've cancelled.
	close(first.done)

	// collect the result, timing out if LoadAll hung
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected error from cancelled context during iteration")
		}
	case <-time.After(time.Second):
		t.Fatal("LoadAll did not return after cancellation")
	}
	if atomic.LoadInt32(&second.loadCalls) != 0 {
		t.Fatalf("second index should not be loaded after cancellation")
	}
}

// concurrentIndex tracks active Save calls and reports if more than one
// happens at the same time (violating serialization guarantees).
type concurrentIndex struct {
	running int32
	errCh   chan error
}

func (c *concurrentIndex) Add(label uint64, vector []float32) error { return nil }
func (c *concurrentIndex) Search(vector []float32, k int) ([]uint64, []float32, error) {
	return nil, nil, nil
}
func (c *concurrentIndex) Save(path string) error {
	if atomic.AddInt32(&c.running, 1) > 1 {
		select {
		case c.errCh <- errors.New("concurrent save"):
		default:
		}
	}
	// simulate some work
	time.Sleep(10 * time.Millisecond)
	atomic.AddInt32(&c.running, -1)
	return nil
}

func (c *concurrentIndex) Load(path string) error {
	if atomic.AddInt32(&c.running, 1) > 1 {
		select {
		case c.errCh <- errors.New("concurrent load/save"):
		default:
		}
	}
	// simulate some work
	time.Sleep(10 * time.Millisecond)
	atomic.AddInt32(&c.running, -1)
	return nil
}

func (c *concurrentIndex) Close() error { return nil }

func TestPersistenceManager_SaveAll_Serializes(t *testing.T) {
	ci := &concurrentIndex{errCh: make(chan error, 1)}
	pm := index.NewPersistenceManager([]index.IndexedFile{{Path: "foo", Index: ci}}, time.Second, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = pm.SaveAll()
	}()
	go func() {
		defer wg.Done()
		_ = pm.SaveAll()
	}()
	wg.Wait()

	select {
	case err := <-ci.errCh:
		t.Fatalf("SaveAll calls overlapped: %v", err)
	default:
		// good
	}
}

func TestPersistenceManager_LoadAllSaveAll_Serializes(t *testing.T) {
	ci := &concurrentIndex{errCh: make(chan error, 1)}
	pm := index.NewPersistenceManager([]index.IndexedFile{{Path: "foo", Index: ci}}, time.Second, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = pm.SaveAll()
	}()
	go func() {
		defer wg.Done()
		_ = pm.LoadAll(context.Background())
	}()
	wg.Wait()

	select {
	case err := <-ci.errCh:
		t.Fatalf("Save/Load calls overlapped: %v", err)
	default:
		// good
	}
}
