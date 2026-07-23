package appstate

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ModeIncremental = "incremental"
	ModeFull        = "full"
)

type IndexingSnapshot struct {
	JobID           string
	Running         bool
	Mode            string
	Scanned         int64
	Indexed         int64
	Skipped         int64
	Deleted         int64
	Representations int64
	ChunksTotal     int64
	EmbeddedOK      int64
	Errors          int64
	Unknown         int64
	// WatchActive reports whether a filesystem watcher is running this process.
	// When false (a one-shot index, or a platform/config without a watcher),
	// WatchOverflows is meaningless and callers MUST omit the optional
	// watch_overflows stats field rather than report 0 (spec bs-007 / SPEC §15.6,
	// #591): absence means "unknown / not applicable", 0 means "watching, none
	// dropped".
	WatchActive    bool
	WatchOverflows int64
}

type IndexingState struct {
	// snap is a snapshot barrier, used INVERTED relative to the usual
	// reader/writer split, and the split is by mutation SHAPE, not by
	// read-vs-write:
	//
	//   - Single-field mutators (every Add*/Set* that touches exactly one
	//     field) take RLock. Each is a single atomic operation, so they are
	//     safe to run concurrently with one another; RLock lets them all
	//     through and costs a bare atomic add on the uncontended path.
	//   - COMPOUND mutations that write more than one field — ResetProgress
	//     (seven counters) and AddWatchOverflow (watchActive + watchOverflows)
	//     — take the exclusive Lock. RLock would let them interleave with a
	//     single-field mutator and leave a DURABLE invalid state: e.g. an
	//     AddIndexed landing between ResetProgress's indexed.Store(0) and
	//     scanned.Store(0) persists indexed>0 with scanned=0, which no reader
	//     can un-see. Exclusion is what keeps a compound write all-or-nothing.
	//   - Snapshot takes the exclusive Lock too: it reads thirteen fields one
	//     at a time and must not observe a state no mutator ever produced.
	//
	// Without this a status scrape could report indexed+skipped+errors >
	// scanned — numbers that never existed (#426 lineage).
	snap sync.RWMutex

	jobID   atomic.Value
	mode    atomic.Value
	running atomic.Bool

	scanned         atomic.Int64
	indexed         atomic.Int64
	skipped         atomic.Int64
	deleted         atomic.Int64
	representations atomic.Int64
	chunksTotal     atomic.Int64
	embeddedOK      atomic.Int64
	errors          atomic.Int64

	// watchActive/watchOverflows are process-lifetime watcher telemetry, not
	// per-scan progress: ResetProgress leaves them untouched (like embeddedOK)
	// so the overflow tally survives rescans.
	watchActive    atomic.Bool
	watchOverflows atomic.Int64
}

func NewIndexingState(mode string) *IndexingState {
	s := &IndexingState{}
	s.jobID.Store(defaultJobID())
	s.mode.Store(normalizeMode(mode))
	return s
}

func (s *IndexingState) SetJobID(jobID string) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		jobID = defaultJobID()
	}
	s.jobID.Store(jobID)
}

func (s *IndexingState) SetMode(mode string) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.mode.Store(normalizeMode(mode))
}

func (s *IndexingState) SetRunning(running bool) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.running.Store(running)
}

// ResetProgress zeroes the per-run progress counters that runScan owns so a new
// scan reports the current corpus rather than an ever-growing sum of every scan
// the daemon has performed (issue #426). It deliberately leaves embeddedOK
// untouched: that counter is owned by the (separately running, resumable) embed
// worker and preloaded from the store, so it reflects durable embed progress
// rather than a single scan and must survive across rescans. jobID/mode/running
// are lifecycle fields, not counters, and are left as-is.
//
// ResetProgress is a COMPOUND mutation (seven counters) and therefore takes the
// exclusive snap lock rather than a shared RLock: under RLock a concurrent
// AddIndexed could land between indexed.Store(0) and scanned.Store(0) and
// persist indexed>0 with scanned=0 — a durable invariant violation, not merely
// a torn read. Exclusion makes the reset's own writes atomic against Snapshot
// and against any single concurrent mutator.
//
// Note the boundary it does NOT police: a caller that records a file as two
// separate calls (AddScanned then AddIndexed) and whose reset fires between them
// can still leave indexed>scanned, because that straddle is in the caller, not
// inside this method. That is a non-issue in practice — ResetProgress runs at
// scan start on the same goroutine that owns the scan's increments, so it is
// never concurrent with them. The component-before-scanned ordering below is
// now redundant belt-and-braces and no longer load-bearing.
func (s *IndexingState) ResetProgress() {
	if s == nil {
		return
	}
	s.snap.Lock()
	defer s.snap.Unlock()
	s.indexed.Store(0)
	s.skipped.Store(0)
	s.deleted.Store(0)
	s.representations.Store(0)
	s.chunksTotal.Store(0)
	s.errors.Store(0)
	s.scanned.Store(0)
}

func (s *IndexingState) AddScanned(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.scanned.Add(delta)
}

func (s *IndexingState) AddIndexed(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.indexed.Add(delta)
}

func (s *IndexingState) AddSkipped(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.skipped.Add(delta)
}

func (s *IndexingState) AddDeleted(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.deleted.Add(delta)
}

func (s *IndexingState) AddRepresentations(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.representations.Add(delta)
}

func (s *IndexingState) AddChunksTotal(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.chunksTotal.Add(delta)
}

func (s *IndexingState) AddEmbeddedOK(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.embeddedOK.Add(delta)
}

func (s *IndexingState) AddErrors(delta int64) {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.errors.Add(delta)
}

// MarkWatchActive records that a filesystem watcher is running this process, so
// Snapshot reports watch_overflows (even 0) instead of omitting it. Idempotent;
// call it once when the watch loop starts.
func (s *IndexingState) MarkWatchActive() {
	if s == nil {
		return
	}
	s.snap.RLock()
	defer s.snap.RUnlock()
	s.watchActive.Store(true)
}

// AddWatchOverflow increments the process-lifetime count of fsnotify kernel
// event-buffer overflows (dropped-event bursts reconciled by the safety rescan
// rather than per-event). It also marks the watcher active: an overflow can only
// be observed while watching. Because it writes TWO fields it is a compound
// mutation and takes the exclusive snap lock, so Snapshot can never observe
// watchOverflows advanced while watchActive is still its old value (or vice
// versa).
func (s *IndexingState) AddWatchOverflow(delta int64) {
	if s == nil {
		return
	}
	s.snap.Lock()
	defer s.snap.Unlock()
	s.watchActive.Store(true)
	s.watchOverflows.Add(delta)
}

func (s *IndexingState) Snapshot() IndexingSnapshot {
	if s == nil {
		return IndexingSnapshot{
			JobID: defaultJobID(),
			Mode:  ModeIncremental,
		}
	}

	s.snap.Lock()
	defer s.snap.Unlock()

	return IndexingSnapshot{
		JobID:           loadString(&s.jobID, defaultJobID()),
		Running:         s.running.Load(),
		Mode:            loadString(&s.mode, ModeIncremental),
		Scanned:         s.scanned.Load(),
		Indexed:         s.indexed.Load(),
		Skipped:         s.skipped.Load(),
		Deleted:         s.deleted.Load(),
		Representations: s.representations.Load(),
		ChunksTotal:     s.chunksTotal.Load(),
		EmbeddedOK:      s.embeddedOK.Load(),
		Errors:          s.errors.Load(),
		WatchActive:     s.watchActive.Load(),
		WatchOverflows:  s.watchOverflows.Load(),
	}
}

func loadString(value *atomic.Value, fallback string) string {
	raw := value.Load()
	cast, ok := raw.(string)
	if !ok || strings.TrimSpace(cast) == "" {
		return fallback
	}
	return cast
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeFull:
		return ModeFull
	default:
		return ModeIncremental
	}
}

func defaultJobID() string {
	return fmt.Sprintf("job_%d", time.Now().UTC().UnixNano())
}
