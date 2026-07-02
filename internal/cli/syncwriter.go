package cli

import (
	"io"
	"sync"
)

// syncWriter serializes concurrent writes to an underlying io.Writer with a
// mutex. The server runs several long-lived goroutines (embed workers, the
// corpus writer, the watch worker) plus the main event loop, and they all log
// diagnostics to the same process stderr sink. A *log.Logger guards its own
// Output calls, but direct writef(stderr, ...) writes bypass that lock, so two
// goroutines writing to one unsynchronized sink (e.g. a bytes.Buffer in tests)
// is a data race (issue #419). Routing every concurrent-phase writer through a
// single syncWriter gives them one shared lock.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// newSyncWriter wraps w so concurrent Write calls are serialized. If w is
// already a *syncWriter it is returned unchanged to avoid nesting locks.
func newSyncWriter(w io.Writer) *syncWriter {
	if sw, ok := w.(*syncWriter); ok {
		return sw
	}
	return &syncWriter{w: w}
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
