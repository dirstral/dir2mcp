package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// TestExtractSegmentURL_RequiresHTTPURL pins the URL guard (issue #243):
// ExtractSegmentURL only accepts http(s) inputs (the schemes ffmpeg can
// byte-range-seek), so a non-URL source is rejected before any binary is needed.
func TestExtractSegmentURL_RequiresHTTPURL(t *testing.T) {
	for _, in := range []string{"", "/local/path.mp4", "file:///x.mp4", "s3://bucket/key.mp4", "ftp://h/x"} {
		if _, err := avutil.ExtractSegmentURL(context.Background(), in, ".mp4", 0, 1000); err == nil {
			t.Errorf("ExtractSegmentURL(%q) = nil error, want non-http rejection", in)
		}
	}
}

// TestExtractSegmentURL_InvalidRange validates the half-open window guard runs
// for the URL path too, before any binary is needed.
func TestExtractSegmentURL_InvalidRange(t *testing.T) {
	for _, tc := range []struct{ start, end int }{
		{0, 0}, {5, 5}, {10, 3}, {-1, 5},
	} {
		_, err := avutil.ExtractSegmentURL(context.Background(), "https://h/clip.mp4", ".mp4", tc.start, tc.end)
		if err == nil {
			t.Errorf("ExtractSegmentURL(_, [%d,%d)) = nil error, want range error", tc.start, tc.end)
		}
	}
}

// TestExtractSegmentURL_ToolNotFound confirms a missing ffmpeg is reported
// distinctly for the URL path, matching ExtractSegment, so callers can skip
// gracefully. The guards pass (valid https URL + valid range) so the code reaches
// the ffmpeg LookPath.
func TestExtractSegmentURL_ToolNotFound(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := avutil.ExtractSegmentURL(context.Background(), "https://h/clip.mp4", ".mp4", 0, 1000)
	if !errors.Is(err, avutil.ErrToolNotFound) {
		t.Errorf("ExtractSegmentURL with empty PATH = %v, want ErrToolNotFound", err)
	}
}

// TestExtractSegmentURL_AcceptsHTTPAndHTTPS confirms both http and https pass the
// scheme guard (they fail later only at ffmpeg, which is the next step).
func TestExtractSegmentURL_AcceptsHTTPAndHTTPS(t *testing.T) {
	t.Setenv("PATH", "")
	for _, in := range []string{"http://h/clip.mp4", "https://h/clip.mp4"} {
		_, err := avutil.ExtractSegmentURL(context.Background(), in, ".mp4", 0, 1000)
		// With an empty PATH the scheme guard passed and we reached ffmpeg lookup.
		if err == nil || !errors.Is(err, avutil.ErrToolNotFound) {
			t.Errorf("ExtractSegmentURL(%q) = %v, want to pass scheme guard and reach ErrToolNotFound", in, err)
		}
		if strings.Contains(err.Error(), "requires an http") {
			t.Errorf("ExtractSegmentURL(%q) wrongly rejected a valid scheme: %v", in, err)
		}
	}
}
