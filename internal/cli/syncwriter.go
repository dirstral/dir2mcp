package cli

import (
	"io"
	"sync"
)

// SyncWriter serializes concurrent writes to an underlying io.Writer with a
// mutex. The server runs several long-lived goroutines (embed workers, the
// corpus writer, the watch worker) plus the main event loop, and they all log
// diagnostics to the same process stderr sink. A *log.Logger guards its own
// Output calls, but direct writef(stderr, ...) writes bypass that lock, so two
// goroutines writing to one unsynchronized sink (e.g. a bytes.Buffer in tests)
// is a data race (issue #419). Routing every concurrent-phase writer through a
// single SyncWriter gives them one shared lock.
//
// It is exported (with NewSyncWriter) only so the concurrency regression guard
// can live in tests/ per AGENTS.md rather than as an internal *_test.go file;
// there is no external consumer (internal/cli is not importable outside the
// module).
type SyncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewSyncWriter wraps w so concurrent Write calls are serialized. If w is
// already a *SyncWriter it is returned unchanged to avoid nesting locks.
func NewSyncWriter(w io.Writer) *SyncWriter {
	if sw, ok := w.(*SyncWriter); ok {
		return sw
	}
	return &SyncWriter{w: w}
}

// Write serializes concurrent writes to the wrapped io.Writer under a mutex.
func (s *SyncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
