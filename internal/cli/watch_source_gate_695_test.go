package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

// stubWatchIngestor is the minimum an ingestor needs for startWatchWorker: a
// Run method to satisfy the parameter type, and a Watch method to satisfy the
// watchable assertion. It records that Watch ran.
type stubWatchIngestor struct {
	mu      sync.Mutex
	watched bool
}

func (s *stubWatchIngestor) Run(context.Context) error { return nil }

func (s *stubWatchIngestor) Watch(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = true
	return nil
}

func (s *stubWatchIngestor) didWatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watched
}

// TestStartWatchWorker_WarnsForSourceWithoutFileWatch pins the operator-facing
// half of issue #695.
//
// An operator who sets ingest.watch expects a watcher. For an S3 corpus there is
// no filesystem to watch, so the server runs a periodic reconcile instead. That
// substitution must be visible, or it becomes a quiet failure. The worker still
// starts, because the reconcile is what keeps the remote corpus in sync.
func TestStartWatchWorker_WarnsForSourceWithoutFileWatch(t *testing.T) {
	var out bytes.Buffer
	var wg sync.WaitGroup
	ing := &stubWatchIngestor{}

	startWatchWorker(context.Background(), false, true, "s3", ing, &out, &wg)
	wg.Wait()

	got := out.String()
	for _, want := range []string{"ingest.watch", "source.kind=s3", "periodic rescan"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning must mention %q; got %q", want, got)
		}
	}
	if !ing.didWatch() {
		t.Errorf("the worker must still start for a remote source, so the periodic reconcile runs")
	}
}

// TestStartWatchWorker_NoWarningForFilesystemSources confirms a local or NFS
// corpus is unchanged: it keeps the filesystem watcher and gets no warning.
func TestStartWatchWorker_NoWarningForFilesystemSources(t *testing.T) {
	for _, kind := range []string{"", "local", "nfs"} {
		var out bytes.Buffer
		var wg sync.WaitGroup
		ing := &stubWatchIngestor{}

		startWatchWorker(context.Background(), false, true, kind, ing, &out, &wg)
		wg.Wait()

		if out.Len() != 0 {
			t.Errorf("source.kind=%q is a filesystem corpus and must not warn; got %q", kind, out.String())
		}
		if !ing.didWatch() {
			t.Errorf("source.kind=%q must still start the watcher", kind)
		}
	}
}
