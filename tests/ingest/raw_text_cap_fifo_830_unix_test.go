//go:build unix

package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// The FIFO case lives in a unix-only file. syscall.Mkfifo is not declared on
// Windows, so a runtime.GOOS skip inside the shared file would fail to COMPILE
// there rather than skip: the constraint has to be a build tag.

// TestGenerateRawText_BoundsTheReadOnASourceAStatCannotMeasure is the case a size
// check cannot catch, which is the whole argument of #682 and #830.
//
// The source is a FIFO. os.Stat reports size 0 for it, and the bytes then arrive
// from a later, separate operation that serves as many as the writer sends: the
// stat's number and the read's number are unrelated by construction, exactly as
// they are for a file that grows after discovery or for an object that serves more
// than it listed.
//
// Before the fix this path stat'ed (0, comfortably under any cap), then called an
// unbounded os.ReadFile and swallowed everything the writer produced, and the
// 10 MiB gate afterwards passed too because the payload is smaller than that. With
// the read bounded the pipe delivers at most cap+1 bytes and the document is
// refused.
func TestGenerateRawText_BoundsTheReadOnASourceAStatCannotMeasure(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "grows.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	// The payload is 4x the cap and, deliberately, still under the old hard-coded
	// 10 MiB gate: only a bound on the READ refuses it.
	payload := rawTextTestCap * 4
	writeDone := make(chan int64, 1)
	writeErr := make(chan error, 1)
	go func() {
		// Opening a FIFO for write blocks until a reader opens it, so this open
		// completes when the generator performs its read.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			writeErr <- err
			writeDone <- 0
			return
		}
		defer func() { _ = w.Close() }()
		block := []byte(strings.Repeat("b", 64*1024))
		var sent int64
		for sent < payload {
			n, werr := w.Write(block)
			sent += int64(n)
			if werr != nil {
				// EPIPE once the bounded reader stops and closes: that is the bound
				// working, not a test failure.
				break
			}
		}
		writeDone <- sent
	}()

	st := &fakeRepStore{failAfter: -1}
	rg := newCappedRepGen(t, st, rawTextTestCap)
	doc := model.Document{DocID: 1, RelPath: "grows.txt", DocType: "text"}
	// A read with no bound on a source with no end does not return, so the call is
	// watched: a hang is reported as the failure it is instead of stalling the suite
	// until the package timeout.
	readDone := make(chan error, 1)
	go func() { readDone <- rg.GenerateRawText(context.Background(), doc, fifo) }()
	var err error
	select {
	case err = <-readDone:
	case <-time.After(30 * time.Second):
		t.Fatal("GenerateRawText never returned: the read is not bounded, or no writer attached to the fifo")
	}
	select {
	case werr := <-writeErr:
		t.Fatalf("the fifo writer could not attach: %v", werr)
	default:
	}
	if err == nil {
		t.Fatal("a source that served past the cap must be refused; the read was not bounded")
	}
	if !errors.Is(err, ingest.ErrFileTooLarge) {
		t.Fatalf("error is not tagged ingest.ErrFileTooLarge: %v", err)
	}
	if len(st.chunks) != 0 {
		t.Fatalf("%d chunk(s) were persisted from a refused read, want 0", len(st.chunks))
	}
	// The reader must have stopped near the bound instead of draining the source.
	sent := <-writeDone
	if sent > rawTextTestCap*2 {
		t.Fatalf("the source delivered %d bytes against a cap of %d; the read is not bounded", sent, rawTextTestCap)
	}
}
