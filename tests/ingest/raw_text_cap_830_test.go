package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// These tests cover item 4 of #830: raw-text generation gated on a HARD-CODED
// 10 MiB instead of the configured `ingest.max_file_mb`, and read the file with an
// unbounded os.ReadFile after a stat that constrains nothing.
//
// Two separate defects, so two separate groups of cases below:
//
//  1. the CAP: the operator's configured value is the one enforced now, so a file
//     between 10 MiB and the configured cap is INDEXED rather than refused;
//  2. the READ: it stops at cap+1 bytes, so a source whose size a stat cannot
//     measure is still bounded.

const rawTextTestCap int64 = 1 << 20 // 1 MiB, small enough to keep these tests fast

// newCappedRepGen returns a generator whose raw-text cap is capBytes, plumbed the
// way ingest.NewService plumbs it from ResolvedMaxFileBytes.
func newCappedRepGen(t *testing.T, st model.RepresentationStore, capBytes int64) *ingest.RepresentationGenerator {
	t.Helper()
	rg := ingest.NewRepresentationGenerator(st)
	rg.SetMaxFileBytes(capBytes)
	return rg
}

// writeSizedTextFile writes a file of exactly size bytes of printable text (so it
// is not detected as binary) and returns its path.
func writeSizedTextFile(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sized.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			t.Fatalf("close %s: %v", path, cerr)
		}
	}()
	line := []byte(strings.Repeat("a", 1023) + "\n")
	for written := int64(0); written < size; {
		chunk := line
		if remaining := size - written; remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		n, werr := f.Write(chunk)
		if werr != nil {
			t.Fatalf("write %s: %v", path, werr)
		}
		written += int64(n)
	}
	return path
}

// TestGenerateRawText_AtTheConfiguredCapIsAdmitted is the off-by-one guard: a file
// of exactly `ingest.max_file_mb` is inside the operator's policy, so it must read
// cleanly and produce chunks. A bound that refused AT the cap would fail here.
func TestGenerateRawText_AtTheConfiguredCapIsAdmitted(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := newCappedRepGen(t, st, rawTextTestCap)
	path := writeSizedTextFile(t, rawTextTestCap)

	doc := model.Document{DocID: 1, RelPath: "at-cap.txt", DocType: "text"}
	if err := rg.GenerateRawText(context.Background(), doc, path); err != nil {
		t.Fatalf("a file of exactly the cap must be indexed, got err: %v", err)
	}
	if len(st.chunks) == 0 {
		t.Fatal("no chunks were inserted for a file at the cap")
	}
}

// TestGenerateRawText_OneBytePastTheConfiguredCapIsRefused is the other half of the
// off-by-one: cap+1 is outside the policy and must be refused, tagged with the
// existing ErrFileTooLarge sentinel (§14.4 FILE_TOO_LARGE), and must persist
// nothing.
func TestGenerateRawText_OneBytePastTheConfiguredCapIsRefused(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := newCappedRepGen(t, st, rawTextTestCap)
	path := writeSizedTextFile(t, rawTextTestCap+1)

	doc := model.Document{DocID: 1, RelPath: "past-cap.txt", DocType: "text"}
	err := rg.GenerateRawText(context.Background(), doc, path)
	if err == nil {
		t.Fatal("a file one byte past the cap must be refused")
	}
	if !errors.Is(err, ingest.ErrFileTooLarge) {
		t.Fatalf("error is not tagged ingest.ErrFileTooLarge: %v", err)
	}
	if !strings.Contains(err.Error(), "ingest.max_file_mb") {
		t.Fatalf("error must name the setting the operator has to change, got: %v", err)
	}
	if len(st.chunks) != 0 || len(st.reps) != 0 {
		t.Fatalf("refused file persisted %d chunk(s) and %d representation(s), want 0 and 0", len(st.chunks), len(st.reps))
	}
}

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
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo is not available on windows")
	}
	fifo := filepath.Join(t.TempDir(), "grows.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	// The payload is 4x the cap and, deliberately, still under the old hard-coded
	// 10 MiB gate: only a bound on the READ refuses it.
	payload := rawTextTestCap * 4
	writeDone := make(chan int64, 1)
	go func() {
		// Opening a FIFO for write blocks until a reader opens it, so this is the
		// read the generator performs.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
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
	err := rg.GenerateRawText(context.Background(), doc, fifo)
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

// TestGenerateRawTextFromContent_BetweenTheTwoCapsIsIndexed is item 4's behavior
// decision, stated as a test: with `ingest.max_file_mb` raised above 10 MiB, a file
// larger than the old hard-coded gate but inside the configured cap is INDEXED.
//
// Before the fix this returned FILE_TOO_LARGE, so the operator who raised the
// setting did not get what the setting says. It is a behavior change for those
// deployments, and this is the case that changes.
func TestGenerateRawTextFromContent_BetweenTheTwoCapsIsIndexed(t *testing.T) {
	const hardCodedCap = 10 * 1024 * 1024 // the pre-#830 gate
	const configuredCap = 20 * 1024 * 1024

	st := &fakeRepStore{failAfter: -1}
	rg := newCappedRepGen(t, st, configuredCap)
	// Just past the old gate, comfortably inside the configured cap.
	content := []byte(strings.Repeat("between the two caps\n", (hardCodedCap/21)+64))
	if int64(len(content)) <= hardCodedCap {
		t.Fatalf("fixture is %d bytes, must exceed the old %d-byte gate to be meaningful", len(content), hardCodedCap)
	}

	doc := model.Document{DocID: 1, RelPath: "big-but-allowed.txt", DocType: "text"}
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, content); err != nil {
		t.Fatalf("a file inside the CONFIGURED cap must be indexed, got err: %v", err)
	}
	if len(st.chunks) == 0 {
		t.Fatal("no chunks were inserted for a file inside the configured cap")
	}
}

// TestGenerateRawTextFromContent_PastTheConfiguredCapIsRefused confirms the gate is
// still a gate: content past the configured cap is refused with the existing
// sentinel. A LOWERED cap is used, so a pass cannot be explained by the default.
func TestGenerateRawTextFromContent_PastTheConfiguredCapIsRefused(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := newCappedRepGen(t, st, rawTextTestCap)

	doc := model.Document{DocID: 1, RelPath: "over.txt", DocType: "text"}
	err := rg.GenerateRawTextFromContent(context.Background(), doc, make([]byte, rawTextTestCap+1))
	if !errors.Is(err, ingest.ErrFileTooLarge) {
		t.Fatalf("content past the configured cap must be refused with ErrFileTooLarge, got: %v", err)
	}
	if len(st.chunks) != 0 {
		t.Fatalf("%d chunk(s) were persisted past the cap, want 0", len(st.chunks))
	}
}
