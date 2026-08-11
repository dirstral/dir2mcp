package ingest

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests below cover issue #679: a debounced change that the internal job
// queue refuses was dropped in silence, so the corpus served stale or missing
// content until the next safety rescan tick, up to ten minutes later.
//
// They are deterministic. The queue is created with capacity 1 and filled
// before the change is armed, so the very next send MUST hit the full-queue
// path. Nothing depends on how fast a document is indexed, and no worker runs.

// newSaturatedWatchLoop returns a watch loop whose job queue is already full,
// plus the log sink and the rescan-request recorder the tests assert on.
func newSaturatedWatchLoop(t *testing.T) (*fsWatchLoop, *syncBuffer, *rescanRecorder) {
	t.Helper()
	logs := &syncBuffer{}
	svc := &Service{}
	svc.SetLogger(log.New(logs, "", 0))

	rescans := newRescanRecorder()
	w := &fsWatchLoop{
		svc:           svc,
		debounce:      time.Millisecond,
		pending:       make(map[string]*time.Timer),
		fire:          make(chan watchJob, 1),
		requestRescan: rescans.request,
	}
	// Fill the queue. The worker is not running, so it stays full for the whole
	// test and every armed job must take the drop path.
	w.fire <- watchJob{absPath: "/corpus/blocking.md"}
	return w, logs, rescans
}

// TestWatchQueueFull_DropAsksForImmediateRescan is the core assertion: a change
// the queue refused must schedule the reconcile that repairs it.
func TestWatchQueueFull_DropAsksForImmediateRescan(t *testing.T) {
	w, logs, rescans := newSaturatedWatchLoop(t)

	w.arm("/corpus/dropped.md", false)

	if !rescans.wait(2 * time.Second) {
		t.Fatalf("a change dropped by a full job queue asked for no rescan; the corpus stays stale until the %v safety rescan", safetyRescanInterval)
	}
	waitForLog(t, logs, "internal job queue full")
	if got := logs.String(); !strings.Contains(got, "/corpus/dropped.md") {
		t.Fatalf("a dropped change must be reported with its path; log was %q", got)
	}
}

// TestWatchQueueFull_DeleteDropAsksForImmediateRescan covers the other job kind.
// A dropped removal leaves deleted content searchable, which is the worse half
// of the defect, so it must reach the reconcile path too.
func TestWatchQueueFull_DeleteDropAsksForImmediateRescan(t *testing.T) {
	w, logs, rescans := newSaturatedWatchLoop(t)

	w.arm("/corpus/removed.md", true)

	if !rescans.wait(2 * time.Second) {
		t.Fatalf("a dropped removal asked for no rescan; the deleted file stays searchable until the %v safety rescan", safetyRescanInterval)
	}
	waitForLog(t, logs, "internal job queue full")
	if got := logs.String(); !strings.Contains(got, "/corpus/removed.md") {
		t.Fatalf("a dropped removal must be reported with its path; log was %q", got)
	}
}

// TestWatchQueueFull_BurstReportsOnceAndReconcilesAll asserts the burst
// behavior an operator meets in practice: every drop asks for the reconcile, the
// requests coalesce, and the log carries a running total instead of one line per
// dropped path.
func TestWatchQueueFull_BurstReportsOnceAndReconcilesAll(t *testing.T) {
	w, logs, rescans := newSaturatedWatchLoop(t)

	const burst = 40
	for i := 0; i < burst; i++ {
		w.arm(fmt.Sprintf("/corpus/burst-%d.md", i), false)
	}
	if !rescans.wait(2 * time.Second) {
		t.Fatalf("a burst of %d dropped changes asked for no rescan", burst)
	}
	waitForRescanRequests(t, rescans, burst)

	if got := rescans.calls(); got != burst {
		t.Errorf("rescan requests = %d, want one per dropped change (%d)", got, burst)
	}
	lines := logLinesWith(logs.String(), "internal job queue full")
	if len(lines) != 1 {
		t.Fatalf("want exactly one queue-full line for one burst, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "change(s) dropped since the watcher started") {
		t.Errorf("the line must name a running total; line was %q", lines[0])
	}
	if !strings.Contains(lines[0], "rescan") {
		t.Errorf("the line must say what closes the gap; line was %q", lines[0])
	}
}

// TestWatchQueueFull_ReconcileReportsTheBurstSize asserts the second half of the
// operator's view: the rescan that repairs the burst names how many changes it
// covers, so the size of the loss is on the record and not only the first drop.
func TestWatchQueueFull_ReconcileReportsTheBurstSize(t *testing.T) {
	w, logs, rescans := newSaturatedWatchLoop(t)

	const burst = 7
	for i := 0; i < burst; i++ {
		w.arm(fmt.Sprintf("/corpus/burst-%d.md", i), false)
	}
	waitForRescanRequests(t, rescans, burst)

	// Drive the worker over one queued rescan request. The service holds no
	// store, so the scan itself fails at once; the report must already be out.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rescanReq := make(chan struct{}, 1)
	rescanReq <- struct{}{}
	done := make(chan struct{})
	go w.worker(ctx, rescanReq, done)
	waitForLog(t, logs, "reconciles 7 change(s)")
	cancel()
	<-done
}

// TestWatchQueueFull_RoomInQueueKeepsPerPathProcessing guards the normal path:
// while the queue has room, a change is still handed to the worker and no
// reconcile is asked for.
func TestWatchQueueFull_RoomInQueueKeepsPerPathProcessing(t *testing.T) {
	w, logs, rescans := newSaturatedWatchLoop(t)
	<-w.fire // make room

	w.arm("/corpus/normal.md", false)

	var job watchJob
	select {
	case job = <-w.fire:
	case <-time.After(2 * time.Second):
		t.Fatalf("an armed change never reached the worker queue")
	}
	if job.absPath != "/corpus/normal.md" || job.deleted {
		t.Fatalf("queued job = %+v, want the armed write for /corpus/normal.md", job)
	}
	if got := rescans.calls(); got != 0 {
		t.Errorf("rescan requests = %d, want 0: a queued change needs no reconcile", got)
	}
	if got := logs.String(); got != "" {
		t.Errorf("a queued change must not be reported as a drop; log was %q", got)
	}
}

// waitForRescanRequests waits until want drops have asked for a reconcile, so
// the assertions read a settled burst rather than a partly armed one. The drop
// path counts the drop before it asks, so this also means want drops are counted.
func waitForRescanRequests(t *testing.T, rescans *rescanRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := rescans.calls()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rescan requests = %d, want %d; a dropped change that asks for nothing is a silent drop", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForLog waits until the captured log holds want. A log line is written
// after the counter moves, so a test that asserts on text must wait for the text.
func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := logs.String()
		if strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log never reported %q; log was %q", want, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// logLinesWith returns the captured log lines that hold want.
func logLinesWith(logged, want string) []string {
	var out []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, want) {
			out = append(out, line)
		}
	}
	return out
}

// rescanRecorder stands in for the coalesced rescan request slot run() installs.
// It counts every request and signals the first one.
type rescanRecorder struct {
	mu    sync.Mutex
	count int
	first chan struct{}
}

func newRescanRecorder() *rescanRecorder {
	return &rescanRecorder{first: make(chan struct{}, 1)}
}

func (r *rescanRecorder) request() {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	select {
	case r.first <- struct{}{}:
	default:
	}
}

func (r *rescanRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func (r *rescanRecorder) wait(d time.Duration) bool {
	select {
	case <-r.first:
		return true
	case <-time.After(d):
		return false
	}
}

// syncBuffer is a log sink that is safe to write from a debounce timer
// goroutine and read from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
