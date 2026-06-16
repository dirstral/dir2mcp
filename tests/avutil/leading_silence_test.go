package tests

import (
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// TestParseLeadingSilence covers the silencedetect stderr parsing that drives
// the optional leading-silence transcript trim (dir2mcp#258). The detector
// itself shells out to ffmpeg; parsing is the deterministic part worth testing
// without the binary.
func TestParseLeadingSilence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		stderr string
		want   time.Duration
	}{
		{
			name: "leading silence at exact zero",
			stderr: "[silencedetect @ 0x1] silence_start: 0\n" +
				"[silencedetect @ 0x1] silence_end: 1.234 | silence_duration: 1.234\n",
			want: 1234 * time.Millisecond,
		},
		{
			name: "leading silence within start epsilon",
			stderr: "[silencedetect @ 0x1] silence_start: 0.012\n" +
				"[silencedetect @ 0x1] silence_end: 2.5 | silence_duration: 2.488\n",
			want: 2500 * time.Millisecond,
		},
		{
			name: "first silence is mid-stream -> no leading silence",
			stderr: "[silencedetect @ 0x1] silence_start: 12.0\n" +
				"[silencedetect @ 0x1] silence_end: 14.0 | silence_duration: 2.0\n",
			want: 0,
		},
		{
			name:   "no silence reported -> zero",
			stderr: "size=N/A time=00:01:00.00 bitrate=N/A speed=120x\n",
			want:   0,
		},
		{
			name: "implausibly long leading silence is rejected",
			stderr: "[silencedetect @ 0x1] silence_start: 0\n" +
				"[silencedetect @ 0x1] silence_end: 42.0 | silence_duration: 42.0\n",
			want: 0,
		},
		{
			name: "leading silence then later gap takes only the leading one",
			stderr: "[silencedetect @ 0x1] silence_start: 0\n" +
				"[silencedetect @ 0x1] silence_end: 0.9 | silence_duration: 0.9\n" +
				"[silencedetect @ 0x1] silence_start: 30.0\n" +
				"[silencedetect @ 0x1] silence_end: 31.0 | silence_duration: 1.0\n",
			want: 900 * time.Millisecond,
		},
		{
			name:   "empty output -> zero",
			stderr: "",
			want:   0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := avutil.ParseLeadingSilence(tc.stderr)
			if got != tc.want {
				t.Fatalf("ParseLeadingSilence = %v, want %v", got, tc.want)
			}
		})
	}
}
