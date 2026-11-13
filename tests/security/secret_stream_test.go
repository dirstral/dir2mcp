package tests

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

// SPEC §7.2 (spec 0.93.0): open_file judges its source in windows. The window
// decision must equal the whole-text decision, so a read boundary can never
// release bytes that the whole-text matcher refuses.

// streamVerdict feeds text to a Stream in pieces cut at the given boundaries
// and returns the final decision.
func streamVerdict(m *secretmatch.Matcher, text []byte, cuts []int) bool {
	st := m.NewStream()
	prev := 0
	for _, c := range cuts {
		if c <= prev || c >= len(text) {
			continue
		}
		if st.Write(text[prev:c]) {
			return true
		}
		prev = c
	}
	if st.Write(text[prev:]) {
		return true
	}
	return st.Close()
}

// TestSecretStream_AnchoredAllowAtTheWindowEnd is the construction from the
// review of PR #1167. The allow pattern ^token=A+$ covers the whole first
// window, but the text goes on past the run of A's, so the whole-text matcher
// finds no allow match and refuses. A read boundary inside the run must not
// release it.
func TestSecretStream_AnchoredAllowAtTheWindowEnd(t *testing.T) {
	tok := "to" + "ken"
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`^` + tok + `=A+$`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	refused := []byte(tok + "=" + strings.Repeat("A", 100000) + "!")
	if !m.Match(refused) {
		t.Fatal("whole-text matcher must refuse the source with the trailing '!'")
	}
	for _, cut := range []int{10, 64 << 10, 70000, 100000, len(refused) - 1} {
		if !streamVerdict(m, refused, []int{cut}) {
			t.Errorf("boundary at %d released a source the whole-text matcher refuses", cut)
		}
	}
	// One byte at a time is the hardest case for the kept text.
	short := append([]byte(tok+"="+strings.Repeat("A", 30000)), '!')
	if !streamVerdict(m, short, everyByte(len(short))) {
		t.Error("byte-by-byte stream released a source the whole-text matcher refuses")
	}

	served := []byte(tok + "=" + strings.Repeat("A", 100000))
	if m.Match(served) {
		t.Fatal("whole-text matcher must serve the source that is one allow match")
	}
	for _, cut := range []int{10, 64 << 10, 70000} {
		if streamVerdict(m, served, []int{cut}) {
			t.Errorf("boundary at %d refused a source the whole-text matcher serves", cut)
		}
	}
}

func everyByte(n int) []int {
	cuts := make([]int, n)
	for i := range cuts {
		cuts[i] = i + 1
	}
	return cuts
}

// TestSecretStream_PendingPastTheRetainBoundRefuses pins the bound: a decision
// that needs more kept text than the Stream may hold is a match.
func TestSecretStream_PendingPastTheRetainBoundRefuses(t *testing.T) {
	tok := "to" + "ken"
	m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`^` + tok + `=A+$`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	st := m.NewStream()
	st.SetMaxRetain(1 << 10)
	if st.Write([]byte(tok + "=" + strings.Repeat("A", 10000))) {
		return
	}
	if !st.Write([]byte(strings.Repeat("A", 10000))) {
		t.Fatal("a pending decision past the retain bound did not refuse")
	}
}

// TestSecretStream_RandomBoundariesEqualWholeText compares the windowed
// decision with the whole-text decision over random texts and random read
// boundaries, for the shipped defaults and for operator allow patterns with an
// anchor and with a suffix.
func TestSecretStream_RandomBoundariesEqualWholeText(t *testing.T) {
	tok := "to" + "ken"
	upperTok := strings.ToUpper(tok)
	cfg := config.Default()
	defaults, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		t.Fatalf("compile defaults: %v", err)
	}
	anchored, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`^` + tok + `=A+$`})
	if err != nil {
		t.Fatalf("compile anchored: %v", err)
	}
	suffix, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`feature_` + tok + `=[A-Za-z0-9]+;ok`})
	if err != nil {
		t.Fatalf("compile suffix: %v", err)
	}

	pieces := []string{
		"plain prose about the regional budget and the new bridge. ",
		"\n",
		"https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbWRkRThBZV" + fakeRun(40) + "&q=https%3A%2F%2Fexample.org ",
		"https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4MTIz==&q=x ",
		"https://www.googleapis.com/youtube/v3/search?part=snippet&page" + "Token=" + fakeRun(24) + " ",
		"https://example.org/list?page_" + tok + "=" + fakeRun(3000) + " ",
		"https://example.org/news/krasnoyar" + "s" + "k-podpisanie-dogovorov-o-sotrudnichestve ",
		"GITHUB_" + upperTok + "=" + fakeRun(40) + "\n",
		"https://example.org/list?page_" + tok + "=abc?GITHUB_" + upperTok + "=" + fakeRun(40) + " ",
		"https://example.org/list?page_" + tok + "=abc,GITHUB_" + upperTok + ":" + fakeRun(40) + " ",
		"feature_" + tok + "=" + fakeRun(30) + ";ok ",
		"feature_" + tok + "=" + fakeRun(30) + ";no ",
		tok + "=" + strings.Repeat("A", 5000),
		"!",
	}
	benign := []int{0, 1, 2, 3, 4, 5, 6, 10}

	rng := rand.New(rand.NewSource(1167))
	names := []string{"defaults", "anchored", "suffix"}
	matchers := []*secretmatch.Matcher{defaults, anchored, suffix}
	var hits, misses int
	for trial := 0; trial < 120; trial++ {
		var b strings.Builder
		if rng.Intn(5) == 0 {
			// A text that is one allow match for the anchored pattern, or one
			// that runs on past it.
			b.WriteString(tok + "=" + strings.Repeat("A", 1+rng.Intn(150000)))
			if rng.Intn(2) == 0 {
				b.WriteString("!")
			}
		} else {
			count := 1 + rng.Intn(12)
			for i := 0; i < count; i++ {
				idx := benign[rng.Intn(len(benign))]
				if rng.Intn(4) == 0 {
					idx = rng.Intn(len(pieces))
				}
				b.WriteString(pieces[idx])
				if rng.Intn(3) == 0 {
					b.WriteString(strings.Repeat("filler text ", rng.Intn(800)))
				}
			}
		}
		text := []byte(b.String())
		for i, m := range matchers {
			want := m.Match(text)
			if want {
				hits++
			} else {
				misses++
			}
			for round := 0; round < 4; round++ {
				cuts := randomCuts(rng, len(text))
				if got := streamVerdict(m, text, cuts); got != want {
					t.Fatalf("%s trial %d: windowed=%v whole-text=%v, len=%d, cuts=%v", names[i], trial, got, want, len(text), cuts)
				}
			}
		}
	}
	if hits == 0 || misses == 0 {
		t.Fatalf("random corpus is one-sided: %d hits, %d misses", hits, misses)
	}
}

// randomCuts returns sorted read boundaries: a run of small reads, reads of
// the open_file chunk size, or random positions.
func randomCuts(rng *rand.Rand, n int) []int {
	if n < 2 {
		return nil
	}
	var cuts []int
	switch rng.Intn(3) {
	case 0:
		step := 1 + rng.Intn(4096)
		for c := step; c < n; c += 1 + rng.Intn(2*step) {
			cuts = append(cuts, c)
		}
	case 1:
		for c := 64 << 10; c < n; c += 64 << 10 {
			cuts = append(cuts, c)
		}
		cuts = append(cuts, 1+rng.Intn(n-1))
	default:
		for i := 0; i < 1+rng.Intn(20); i++ {
			cuts = append(cuts, 1+rng.Intn(n-1))
		}
	}
	sort.Ints(cuts)
	return cuts
}

// TestSecretMatch_ManyAllowedLinksIsFast pins the cover lookup cost: a text
// with 100k allowed links, each holding one generic token match, must not cost
// a scan of every allow span per secret match.
func TestSecretMatch_ManyAllowedLinksIsFast(t *testing.T) {
	m := defaultMatcher(t)
	tok := "to" + "ken"
	link := "https://example.org/list?page_" + tok + "=" + fakeRun(24) + "\n"
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

// BenchmarkSecretMatch_ManyAllowedLinks measures Match over 100k allowed
// links.
func BenchmarkSecretMatch_ManyAllowedLinks(b *testing.B) {
	cfg := config.Default()
	m, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		b.Fatalf("compile: %v", err)
	}
	link := "https://example.org/list?page_" + "to" + "ken=" + fakeRun(24) + "\n"
	text := []byte(strings.Repeat(link, 100000))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if m.Match(text) {
			b.Fatal("matched")
		}
	}
}
