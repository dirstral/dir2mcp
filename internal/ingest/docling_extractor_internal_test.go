package ingest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLimitedBuffer_OverCapNeverErrors is the core regression guard for issue
// #406: when wired as cmd.Stdout/cmd.Stderr, a Write that returns an error
// makes os/exec stop draining the pipe and the child deadlocks on a full pipe.
// Over-cap writes must therefore always report the full length and a nil error,
// retain only up to the cap, and flag truncation.
func TestLimitedBuffer_OverCapNeverErrors(t *testing.T) {
	lb := &limitedBuffer{buf: &bytes.Buffer{}, limit: 8}

	n, err := lb.Write([]byte("1234"))
	if err != nil || n != 4 {
		t.Fatalf("under-cap write: n=%d err=%v", n, err)
	}
	if lb.Truncated() {
		t.Fatal("did not expect truncation under cap")
	}

	// Straddles the cap: 4 bytes already buffered, writing 8 more (total 12 > 8).
	n, err = lb.Write([]byte("ABCDEFGH"))
	if err != nil {
		t.Fatalf("straddling write returned error (would deadlock os/exec): %v", err)
	}
	if n != 8 {
		t.Fatalf("straddling write must report full length to keep pipe draining, got n=%d", n)
	}
	if !lb.Truncated() {
		t.Fatal("expected truncation flag after exceeding cap")
	}

	// Fully past the cap: still accepted, still discarded.
	n, err = lb.Write([]byte("xyz"))
	if err != nil || n != 3 {
		t.Fatalf("over-cap write: n=%d err=%v", n, err)
	}

	if got := lb.buf.Len(); got != 8 {
		t.Fatalf("buffer retained %d bytes, want cap of 8", got)
	}
	if got := lb.buf.String(); got != "1234ABCD" {
		t.Fatalf("retained content = %q, want %q", got, "1234ABCD")
	}
}

// TestLimitedBuffer_ZeroLimitUnbounded confirms a non-positive limit disables
// capping entirely (used where the buffer must keep everything).
func TestLimitedBuffer_ZeroLimitUnbounded(t *testing.T) {
	lb := &limitedBuffer{buf: &bytes.Buffer{}, limit: 0}
	big := strings.Repeat("z", 10_000)
	n, err := lb.Write([]byte(big))
	if err != nil || n != len(big) {
		t.Fatalf("unbounded write: n=%d err=%v", n, err)
	}
	if lb.Truncated() || lb.buf.Len() != len(big) {
		t.Fatalf("unbounded buffer should retain all bytes, got len=%d truncated=%v", lb.buf.Len(), lb.Truncated())
	}
}

// TestRunFileOutput_TimesOutOnHungCommand verifies the per-document timeout
// (issue #406): a docling command that hangs forever is killed and surfaces a
// timeout error instead of wedging the indexer. The fake command both sleeps
// and floods stderr, exercising the deadlock fix and the timeout together.
func TestRunFileOutput_TimesOutOnHungCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	prev := doclingExtractTimeout
	doclingExtractTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doclingExtractTimeout = prev })

	// A fake "docling" that floods stderr (stressing the non-erroring
	// limitedBuffer drain) and then hangs far longer than the timeout. The
	// command-template splitter is whitespace-delimited, so the body lives in a
	// script file rather than an inline `sh -c` argument.
	dir := t.TempDir()
	fake := filepath.Join(dir, "docling")
	// exec replaces the shell with sleep so killing the child closes the stderr
	// pipe immediately (no orphaned process keeps it open).
	script := "#!/bin/sh\nyes hangingprogress | head -c 5000000 >&2\nexec sleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docling: %v", err)
	}
	// {output} keeps the file-output path.
	ext := NewDoclingExtractor(fake + " --output {output} {input}")

	start := time.Now()
	_, err := ext.Extract(context.Background(), "sample.pdf", []byte("%PDF-1.4 fake"))
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("command was not killed promptly by the timeout (took %s)", elapsed)
	}
}
