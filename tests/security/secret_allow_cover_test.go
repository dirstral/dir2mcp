package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

// TestSecretMatch_ManyAllowedLinksIsFast pins the cover lookup cost: a text
// with 100k allowed links, each holding one generic token match, must not cost
// a scan of every allow span per secret match.
func TestSecretMatch_ManyAllowedLinksIsFast(t *testing.T) {
	m := defaultMatcher(t)
	tok := "to" + "ken"
	link := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=" + fakeRun(24) + "\n"
	text := []byte(strings.Repeat(link, 100000))
	start := time.Now()
	if m.Match(text) {
		t.Fatal("100k allowed links matched")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Match over 100k allowed links took %v", elapsed)
	}
	// A real credential after them is still found.
	withCredential := append(append([]byte(nil), text...), []byte("GITHUB_"+strings.ToUpper(tok)+"="+fakeRun(40))...)
	if !m.Match(withCredential) {
		t.Fatal("a credential after 100k allowed links was released")
	}
}

// TestSecretMatch_OneSpanMustCover pins the cover rule of the sorted index: two
// allow spans that together cover a secret match do not cancel it, while one
// span that starts earlier and ends later does.
func TestSecretMatch_OneSpanMustCover(t *testing.T) {
	tok := "to" + "ken"
	secret := "x_" + tok + "=" + fakeRun(30)
	text := "[" + secret + "]"
	// The two halves of the match, each its own allow span.
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`x_` + tok + `=`, fakeRun(30)})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !m.MatchString(text) {
		t.Error("two allow spans that each cover half of the match cancelled it")
	}
	// One span that covers the whole match and more.
	m, err = secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`\[[^\]]*\]`, `zzz`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if m.MatchString(text) {
		t.Error("one allow span that covers the whole match did not cancel it")
	}
}

// BenchmarkSecretMatch_ManyAllowedLinks measures Match over 100k allowed
// links.
func BenchmarkSecretMatch_ManyAllowedLinks(b *testing.B) {
	cfg := config.Default()
	m, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		b.Fatalf("compile: %v", err)
	}
	link := "https://www.youtube.com/redirect?event=video_description&redir_" + "to" + "ken=" + fakeRun(24) + "\n"
	text := []byte(strings.Repeat(link, 100000))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if m.Match(text) {
			b.Fatal("matched")
		}
	}
}

// TestSecretMatch_OverlappingMatchAfterACancelledOne pins the every-start rule
// (SPEC §7.2, spec 0.99.0): a generic token match that starts inside a
// cancelled match is judged on its own.
func TestSecretMatch_OverlappingMatchAfterACancelledOne(t *testing.T) {
	m := defaultMatcher(t)
	tok := "to" + "ken"
	text := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=" +
		strings.Repeat("A", 20) + "GITHUB_" + strings.ToUpper(tok) + "=" + strings.Repeat("B", 20)
	if !m.MatchString(text) {
		t.Fatal("a match that starts inside a cancelled match was released")
	}
	// Many cancelled matches before it do not hide it either, and the search
	// stays fast.
	link := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=" + fakeRun(24) + "\n"
	long := strings.Repeat(link, 100000) + text
	start := time.Now()
	if !m.MatchString(long) {
		t.Fatal("the overlapping match after 100k allowed links was released")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Match took %v", elapsed)
	}
}

// TestSecretMatch_AllowMatchesThatShareContext pins that two allow matches
// that share a context character each cancel the generic match they cover
// (SPEC §7.2). A search for non-overlapping matches consumes the shared `;`
// in the first match and never sees the second one.
func TestSecretMatch_AllowMatchesThatShareContext(t *testing.T) {
	tok := "to" + "ken"
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`;(` + tok + `=A{20});`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	assign := tok + "=" + strings.Repeat("A", 20)
	if text := ";" + assign + ";" + assign + ";"; m.MatchString(text) {
		t.Errorf("two assignments that share a `;` were not both cancelled: %q", text)
	}
	// A third assignment with no closing context is still a match.
	if text := ";" + assign + ";" + assign + ";" + assign; !m.MatchString(text) {
		t.Errorf("an assignment with no allow match was cancelled: %q", text)
	}
}

// TestSecretMatch_AllowAnchorsKeepTheirMeaning pins that the search for
// overlapping allow matches does not change what ^ and \b mean: a search that
// starts inside the text still sees the text before it.
func TestSecretMatch_AllowAnchorsKeepTheirMeaning(t *testing.T) {
	tok := "to" + "ken"
	assign := tok + "=" + strings.Repeat("A", 20)
	// ^ (no multi-line flag) holds only at the start of the whole text.
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`^;(` + tok + `=A{20});`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if text := ";" + assign + ";" + assign + ";"; !m.MatchString(text) {
		t.Errorf("^ matched inside the text and cancelled the second assignment: %q", text)
	}
	// \b needs a word boundary before the match, also inside the text.
	m, err = secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`\b(` + tok + `=A{20})\b`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if text := "x" + assign; !m.MatchString(text) {
		t.Errorf("\\b matched after a word character: %q", text)
	}
	if text := "x " + assign; m.MatchString(text) {
		t.Errorf("\\b did not match after a space: %q", text)
	}
}

// TestSecretMatch_DenseAllowRuleIsBounded pins that an allow rule that matches
// at almost every byte neither keeps its matches nor releases a generic match
// that none of them covers.
func TestSecretMatch_DenseAllowRuleIsBounded(t *testing.T) {
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`.`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	half := strings.Repeat("a b c d ", 1<<18)
	text := []byte(half + "to" + "ken=" + fakeRun(30) + " " + half)
	start := time.Now()
	if !m.Match(text) {
		t.Fatal("a generic match that no one-byte allow match covers was cancelled")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Match with a dense allow rule took %v", elapsed)
	}
}

// TestSecretMatch_RedactJoinsOverlappingMatches pins that Redact finds the
// matches of every pattern in the original message: with `ABC` and `BCDEF`,
// a replacement of `ABC` first must not leave `DEF` visible.
func TestSecretMatch_RedactJoinsOverlappingMatches(t *testing.T) {
	m, err := secretmatch.Compile([]string{`ABC`, `BCDEF`}, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got, want := m.Redact("xxABCDEFyy"), "xx[REDACTED]yy"; got != want {
		t.Errorf("Redact(overlap) = %q, want %q", got, want)
	}
	if got, want := m.Redact("ABC-BCDEF"), "[REDACTED]-[REDACTED]"; got != want {
		t.Errorf("Redact(separate) = %q, want %q", got, want)
	}
	if got := m.Redact("nothing here"); got != "nothing here" {
		t.Errorf("Redact(no match) = %q", got)
	}
}
