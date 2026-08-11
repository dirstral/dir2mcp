package retrieval

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
)

// The streaming selectors replaced buffered ones that split the whole document
// into a string slice (issue #690). The reference implementations below are the
// buffered originals. The tests compare the two over many generated documents,
// so the streaming rewrite keeps the span semantics it inherited.

func refSliceLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = start
	}
	return strings.Join(lines[start-1:end], "\n")
}

func refSlicePage(content string, page int) (string, bool) {
	if page <= 0 {
		page = 1
	}
	parts := strings.Split(content, "\f")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 1 {
		if page > len(parts) {
			return "", false
		}
		return strings.Trim(parts[page-1], "\n"), true
	}
	if page == 1 {
		return parts[0], true
	}
	return "", false
}

func refSliceTime(content string, startMS, endMS int) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	found := false
	for _, line := range lines {
		m := timePrefixRe.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		found = true
		ts := parseTimestampMS(m[1], m[2], m[3])
		if ts < startMS || (endMS > 0 && ts > endMS) {
			continue
		}
		out = append(out, line)
	}
	if !found {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

// TestSelectLineRange_MatchesBufferedSemantics compares the streaming line
// selector with the buffered original over generated documents.
func TestSelectLineRange_MatchesBufferedSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(690))
	pieces := []string{"", "a", "line text", "\n", "b\n", "  ", "\f"}
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for j := 0; j < rng.Intn(12); j++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
			if rng.Intn(2) == 0 {
				b.WriteString("\n")
			}
		}
		content := b.String()
		start := rng.Intn(8) - 1
		end := rng.Intn(8) - 1
		got, err := selectLineRange(strings.NewReader(content), start, end, testSelectBudget)
		if err != nil {
			t.Fatalf("selectLineRange(%q, %d, %d): %v", content, start, end, err)
		}
		if want := refSliceLines(content, start, end); got.text != want {
			t.Fatalf("selectLineRange(%q, %d, %d) = %q, want %q", content, start, end, got.text, want)
		}
	}
}

// TestSelectPage_MatchesBufferedSemantics compares the streaming page selector
// with the buffered original, including the page-count edge cases of #427.
func TestSelectPage_MatchesBufferedSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(427))
	pieces := []string{"", "p", "text\n", "\n", "\f", "a\fb"}
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for j := 0; j < rng.Intn(10); j++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		content := b.String()
		page := rng.Intn(5)
		got, err := selectPage(strings.NewReader(content), page, testSelectBudget)
		wantText, wantOK := refSlicePage(content, page)
		if err != nil {
			if !errors.Is(err, model.ErrDocTypeUnsupported) {
				t.Fatalf("selectPage(%q, %d): %v", content, page, err)
			}
			if wantOK {
				t.Fatalf("selectPage(%q, %d) reported out of range, want %q", content, page, wantText)
			}
			continue
		}
		if !wantOK {
			t.Fatalf("selectPage(%q, %d) = %q, want out of range", content, page, got.text)
		}
		if got.text != wantText {
			t.Fatalf("selectPage(%q, %d) = %q, want %q", content, page, got.text, wantText)
		}
	}
}

// TestSelectTimeRange_MatchesBufferedSemantics compares the streaming time
// selector with the buffered original.
func TestSelectTimeRange_MatchesBufferedSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	pieces := []string{"[00:10] intro", "[01:00] middle", "[02:30] later", "no timestamp", ""}
	for i := 0; i < 200; i++ {
		lines := make([]string, 0, 8)
		for j := 0; j < rng.Intn(8); j++ {
			lines = append(lines, pieces[rng.Intn(len(pieces))])
		}
		content := strings.Join(lines, "\n")
		span := model.Span{Kind: "time", StartMS: rng.Intn(3) * 60000, EndMS: rng.Intn(4) * 60000}
		got, err := selectTimeRange(strings.NewReader(content), span, testSelectBudget)
		startMS, endMS := normalizeTimeBounds(span)
		wantText, wantOK := refSliceTime(content, startMS, endMS)
		if err != nil {
			if !errors.Is(err, model.ErrDocTypeUnsupported) {
				t.Fatalf("selectTimeRange(%q): %v", content, err)
			}
			if wantOK {
				t.Fatalf("selectTimeRange(%q) reported unsupported, want %q", content, wantText)
			}
			continue
		}
		if !wantOK {
			t.Fatalf("selectTimeRange(%q) = %q, want unsupported", content, got.text)
		}
		if got.text != wantText {
			t.Fatalf("selectTimeRange(%q) = %q, want %q", content, got.text, wantText)
		}
	}
}

// TestOpenFileReadBudgetBytes_HoldsTheRequestedRunes pins the relationship
// between the character contract and the byte budget: the budget must hold more
// than max_chars runes even when every rune is four bytes long, so the answer is
// never short and a cut rune never reaches the caller.
func TestOpenFileReadBudgetBytes_HoldsTheRequestedRunes(t *testing.T) {
	for _, maxChars := range []int{200, 20000, 50000} {
		budget := openFileReadBudgetBytes(maxChars)
		if budget < maxChars*utf8.UTFMax {
			t.Fatalf("budget %d for %d runes cannot hold four-byte runes", budget, maxChars)
		}
		body := strings.Repeat("𝔘", maxChars+10)
		selection, err := selectPrefix(strings.NewReader(body), budget)
		if err != nil {
			t.Fatalf("selectPrefix: %v", err)
		}
		out, truncated := truncateRunesWithFlag(selection.text, maxChars)
		if got := utf8.RuneCountInString(out); got != maxChars {
			t.Fatalf("answer holds %d runes, want %d", got, maxChars)
		}
		if !truncated {
			t.Fatalf("expected truncated=true for a source longer than %d runes", maxChars)
		}
		if strings.ContainsRune(out, utf8.RuneError) {
			t.Fatalf("answer holds a cut rune")
		}
	}
}

// TestSelectLineRange_OneHugeLineStaysBounded checks that a single line longer
// than the budget is cut instead of buffered. A document can be one line.
func TestSelectLineRange_OneHugeLineStaysBounded(t *testing.T) {
	const budget = 4096
	body := strings.Repeat("z", 1<<20) + "\nsecond line"
	got, err := selectLineRange(strings.NewReader(body), 1, 1, budget)
	if err != nil {
		t.Fatalf("selectLineRange: %v", err)
	}
	if len(got.text) != budget {
		t.Fatalf("retained %d bytes, want the budget of %d", len(got.text), budget)
	}
	if !got.truncated {
		t.Fatalf("expected truncated=true for a line longer than the budget")
	}
}

// TestSelectPrefix_StopsAtTheBudget checks the head selector keeps one budget
// and reports that more content follows.
func TestSelectPrefix_StopsAtTheBudget(t *testing.T) {
	const budget = 1024
	got, err := selectPrefix(strings.NewReader(strings.Repeat("q", 4096)), budget)
	if err != nil {
		t.Fatalf("selectPrefix: %v", err)
	}
	if len(got.text) != budget || !got.truncated {
		t.Fatalf("selectPrefix kept %d bytes (truncated=%v), want %d and true", len(got.text), got.truncated, budget)
	}

	short, err := selectPrefix(strings.NewReader("tiny"), budget)
	if err != nil {
		t.Fatalf("selectPrefix: %v", err)
	}
	if short.text != "tiny" || short.truncated {
		t.Fatalf("selectPrefix on a short source = (%q,%v), want (\"tiny\",false)", short.text, short.truncated)
	}
}
