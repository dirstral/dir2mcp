package retrieval

import (
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// testSelectBudget is a read budget far larger than any fixture below, so these
// cases exercise span semantics rather than truncation.
const testSelectBudget = 1 << 20

// selectPageString runs the streaming page selector over an in-memory document.
// It restores the (text, ok) shape the buffered slicePage had, so the #427
// cases below keep reading as page-count assertions. Page selection now streams
// (issue #690), and these cases pin that the stream counts pages the same way.
func selectPageString(t *testing.T, content string, page int) (string, bool) {
	t.Helper()
	selection, err := selectPage(strings.NewReader(content), page, testSelectBudget)
	if err != nil {
		if errors.Is(err, model.ErrDocTypeUnsupported) {
			return "", false
		}
		t.Fatalf("selectPage(%q, %d) returned err: %v", content, page, err)
	}
	return selection.text, true
}

// selectTimeString runs the streaming time selector over an in-memory
// transcript and restores the (text, ok) shape of the buffered sliceTime.
func selectTimeString(t *testing.T, content string, startMS, endMS int) (string, bool) {
	t.Helper()
	span := model.Span{Kind: "time", StartMS: startMS, EndMS: endMS}
	selection, err := selectTimeRange(strings.NewReader(content), span, testSelectBudget)
	if err != nil {
		if errors.Is(err, model.ErrDocTypeUnsupported) {
			return "", false
		}
		t.Fatalf("selectTimeRange returned err: %v", err)
	}
	return selection.text, true
}

// TestSlicePage_NoPhantomTrailingPage pins #427: a form-feed-terminated document
// ("p1\fp2\f") must expose exactly its real pages. The trailing empty segment
// must not present a phantom page beyond the last, so an out-of-range page index
// errors (ok=false) rather than returning "" with ok=true.
func TestSlicePage_NoPhantomTrailingPage(t *testing.T) {
	content := "page one\fpage two\f" // 2 real pages, terminating form-feed

	if got, ok := selectPageString(t, content, 1); !ok || got != "page one" {
		t.Fatalf("page 1: got (%q,%v), want (%q,true)", got, ok, "page one")
	}
	if got, ok := selectPageString(t, content, 2); !ok || got != "page two" {
		t.Fatalf("page 2: got (%q,%v), want (%q,true)", got, ok, "page two")
	}
	// Page 3 is past the last real page: must be out-of-range, not a phantom.
	if got, ok := selectPageString(t, content, 3); ok || got != "" {
		t.Fatalf("phantom page 3: got (%q,%v), want (\"\",false)", got, ok)
	}
}

// TestSlicePage_SinglePageTrailingFormFeed pins the #427 review fix: a single-page
// document ending in a form-feed must return the clean text, not re-attach the
// stripped \f (the trailing-empty segment leaves one page, so page 1 returns the
// first segment, not the raw content).
func TestSlicePage_SinglePageTrailingFormFeed(t *testing.T) {
	if got, ok := selectPageString(t, "only page\f", 1); !ok || got != "only page" {
		t.Fatalf("single page w/ trailing form-feed: got (%q,%v), want (%q,true)", got, ok, "only page")
	}
	// A plain single-page doc (no form-feed) is unchanged.
	if got, ok := selectPageString(t, "just text", 1); !ok || got != "just text" {
		t.Fatalf("plain single page: got (%q,%v), want (%q,true)", got, ok, "just text")
	}
	// Page 2 of a single-page doc is out of range.
	if got, ok := selectPageString(t, "only page\f", 2); ok || got != "" {
		t.Fatalf("single-page page 2: got (%q,%v), want (\"\",false)", got, ok)
	}
}

// TestSlicePage_MultiPageTrimsNewlines pins the trimming rule a multi-page
// document carries: page text loses its leading and trailing newlines, while a
// single-page document keeps them. The streaming selector must decide this
// without buffering the whole document, so it peeks one byte past the first
// form feed.
func TestSlicePage_MultiPageTrimsNewlines(t *testing.T) {
	if got, ok := selectPageString(t, "\npage one\n\fpage two", 1); !ok || got != "page one" {
		t.Fatalf("multi-page page 1: got (%q,%v), want (%q,true)", got, ok, "page one")
	}
	if got, ok := selectPageString(t, "\nonly page\n", 1); !ok || got != "\nonly page\n" {
		t.Fatalf("single page: got (%q,%v), want (%q,true)", got, ok, "\nonly page\n")
	}
	// An empty middle page is a real page, not a phantom.
	if got, ok := selectPageString(t, "a\f\fb", 2); !ok || got != "" {
		t.Fatalf("empty middle page: got (%q,%v), want (\"\",true)", got, ok)
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

	// End-to-end through the time selector: a >=100-minute cue must be selectable.
	transcript := "[00:10] intro\n[100:30] the hundred-minute mark\n[101:00] just after"
	out, ok := selectTimeString(t, transcript, 100*60*1000, 101*60*1000)
	if !ok {
		t.Fatalf("time selection returned ok=false; expected timestamps to be found")
	}
	if !strings.Contains(out, "[100:30] the hundred-minute mark") {
		t.Fatalf("time selection output %q missing the >=100-minute cue", out)
	}
}
