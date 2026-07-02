package tests

import (
	"bytes"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestSyncWriterSerializesConcurrentWrites is the focused regression guard for
// issue #419: many goroutines writing to one shared sink (here a bytes.Buffer,
// as in the CLI tests) must not race or lose bytes. Run under `go test -race`
// this fails if SyncWriter drops its mutex; even without -race the byte count
// assertion catches a torn Buffer.grow.
func TestSyncWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := cli.NewSyncWriter(&buf)

	const goroutines, perGoroutine = 8, 250
	line := []byte("embed worker started [kind=text]\n")

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, err := w.Write(line); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got, want := bytes.Count(buf.Bytes(), []byte("\n")), goroutines*perGoroutine; got != want {
		t.Fatalf("lost writes under concurrency: got %d lines, want %d", got, want)
	}
}

// TestNewSyncWriterDoesNotDoubleWrap keeps nested locking from creeping in when
// an already-synchronized sink is wrapped again (e.g. both the embed logger and
// the event loop route through the shared sink).
func TestNewSyncWriterDoesNotDoubleWrap(t *testing.T) {
	inner := cli.NewSyncWriter(&bytes.Buffer{})
	if got := cli.NewSyncWriter(inner); got != inner {
		t.Fatalf("NewSyncWriter double-wrapped an existing *SyncWriter: got %p, want %p", got, inner)
	}
}
