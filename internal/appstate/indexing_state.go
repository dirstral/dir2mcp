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
func (s *IndexingState) ResetProgress() {
	if s == nil {
		return
	}
	s.scanned.Store(0)
	s.indexed.Store(0)
	s.skipped.Store(0)
	s.deleted.Store(0)
	s.representations.Store(0)
	s.chunksTotal.Store(0)
	s.errors.Store(0)
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
