package retrieval

import (
	"strings"
	"testing"
)

// TestSlicePage_NoPhantomTrailingPage pins #427: a form-feed-terminated document
// ("p1\fp2\f") must expose exactly its real pages. The trailing empty segment
// from strings.Split must not present a phantom page beyond the last, so an
// out-of-range page index errors (ok=false) rather than returning "" with ok=true.
func TestSlicePage_NoPhantomTrailingPage(t *testing.T) {
	content := "page one\fpage two\f" // 2 real pages, terminating form-feed

	if got, ok := slicePage(content, 1); !ok || got != "page one" {
		t.Fatalf("page 1: got (%q,%v), want (%q,true)", got, ok, "page one")
	}
	if got, ok := slicePage(content, 2); !ok || got != "page two" {
		t.Fatalf("page 2: got (%q,%v), want (%q,true)", got, ok, "page two")
	}
	// Page 3 is past the last real page: must be out-of-range, not a phantom.
	if got, ok := slicePage(content, 3); ok || got != "" {
		t.Fatalf("phantom page 3: got (%q,%v), want (\"\",false)", got, ok)
	}
}

// TestTimePrefixRe_MinutesOver99 pins #427: single-field MM:SS transcript lines
// past 99 minutes (e.g. "[100:30]", "[123:45]") must parse for open_file time
// slicing; the lead field previously capped at 2 digits and silently dropped them.
func TestTimePrefixRe_MinutesOver99(t *testing.T) {
	cases := []struct {
		line   string
		wantMS int
	}{
		{"[100:30] hundred minutes", (100*60 + 30) * 1000},
		{"[123:45] later still", (123*60 + 45) * 1000},
		{"[09:05] under a hundred", (9*60 + 5) * 1000},
		{"[01:02:03] hh:mm:ss still works", (1*3600 + 2*60 + 3) * 1000},
	}
	for _, tc := range cases {
		m := timePrefixRe.FindStringSubmatch(tc.line)
		if len(m) == 0 {
			t.Fatalf("regex did not match %q", tc.line)
		}
		if got := parseTimestampMS(m[1], m[2], m[3]); got != tc.wantMS {
			t.Fatalf("parseTimestampMS(%q) = %d, want %d", tc.line, got, tc.wantMS)
		}
	}

	// End-to-end through sliceTime: a >=100-minute cue must be selectable.
	transcript := "[00:10] intro\n[100:30] the hundred-minute mark\n[101:00] just after"
	out, ok := sliceTime(transcript, 100*60*1000, 101*60*1000)
	if !ok {
		t.Fatalf("sliceTime returned ok=false; expected timestamps to be found")
	}
	if !strings.Contains(out, "[100:30] the hundred-minute mark") {
		t.Fatalf("sliceTime output %q missing the >=100-minute cue", out)
	}
}
