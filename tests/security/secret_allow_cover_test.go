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
// (SPEC §7.2, spec 0.93.0): a generic token match that starts inside a
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
