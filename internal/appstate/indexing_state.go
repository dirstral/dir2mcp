package appstate

import (
	"fmt"
	"strings"
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
	s.mode.Store(normalizeMode(mode))
}

func (s *IndexingState) SetRunning(running bool) {
	if s == nil {
		return
	}
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
// Snapshot() reads each counter independently without a lock, so a concurrent
// status scrape can interleave with this reset. To keep the
// indexed+skipped+errors <= scanned invariant intact for every observer, the
// component counters are zeroed first and scanned is zeroed last: any snapshot
// taken mid-reset then sees a still-large (or already-zero) scanned alongside
// component counters that are only ever <= their pre-reset values, never the
// reverse (scanned=0 while indexed still carries the previous run's total).
func (s *IndexingState) ResetProgress() {
	if s == nil {
		return
	}
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
	s.scanned.Add(delta)
}

func (s *IndexingState) AddIndexed(delta int64) {
	if s == nil {
		return
	}
	s.indexed.Add(delta)
}

func (s *IndexingState) AddSkipped(delta int64) {
	if s == nil {
		return
	}
	s.skipped.Add(delta)
}

func (s *IndexingState) AddDeleted(delta int64) {
	if s == nil {
		return
	}
	s.deleted.Add(delta)
}

func (s *IndexingState) AddRepresentations(delta int64) {
	if s == nil {
		return
	}
	s.representations.Add(delta)
}

func (s *IndexingState) AddChunksTotal(delta int64) {
	if s == nil {
		return
	}
	s.chunksTotal.Add(delta)
}

func (s *IndexingState) AddEmbeddedOK(delta int64) {
	if s == nil {
		return
	}
	s.embeddedOK.Add(delta)
}

func (s *IndexingState) AddErrors(delta int64) {
	if s == nil {
		return
	}
	s.errors.Add(delta)
}

// MarkWatchActive records that a filesystem watcher is running this process, so
// Snapshot reports watch_overflows (even 0) instead of omitting it. Idempotent;
// call it once when the watch loop starts.
func (s *IndexingState) MarkWatchActive() {
	if s == nil {
		return
	}
	s.watchActive.Store(true)
}

// AddWatchOverflow increments the process-lifetime count of fsnotify kernel
// event-buffer overflows (dropped-event bursts reconciled by the safety rescan
// rather than per-event). It also marks the watcher active: an overflow can only
// be observed while watching.
func (s *IndexingState) AddWatchOverflow(delta int64) {
	if s == nil {
		return
	}
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
