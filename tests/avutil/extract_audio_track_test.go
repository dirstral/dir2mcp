package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// TestExtractAudioTrackIndex_NegativeIndexRejected pins that a negative
// audio-relative track index is rejected before any probe/ffmpeg work (SPEC
// §8.6.12): the 0-based grammar has no negative track, so it is a programming
// error, NOT a graceful ErrNoAudioStream. Asserting the specific cause (and that
// it is distinct from ErrNoAudioStream) keeps the test honest even on a machine
// that happens to have ffprobe/ffmpeg installed, where a bare `err != nil` could
// otherwise be satisfied by the nonexistent input file rather than the guard.
func TestExtractAudioTrackIndex_NegativeIndexRejected(t *testing.T) {
	_, err := avutil.ExtractAudioTrackIndex(context.Background(), "does-not-matter.mkv", -1)
	if err == nil {
		t.Fatal("ExtractAudioTrackIndex(..., -1) = nil error, want a rejection")
	}
	if errors.Is(err, avutil.ErrNoAudioStream) {
		t.Fatalf("negative index must be a hard programming error, not the graceful ErrNoAudioStream: %v", err)
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Fatalf("error should name the negative-index cause (the pre-probe guard), got: %v", err)
	}
}
