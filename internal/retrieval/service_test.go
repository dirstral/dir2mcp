package retrieval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dir2mcp/internal/index"
	"dir2mcp/internal/model"
)

type fakeRetrievalEmbedder struct {
	vectorsByModel map[string][]float32
}

type fakeGenerator struct {
	out string
	err error
}

func (g *fakeGenerator) Generate(_ context.Context, _ string) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	return g.out, nil
}

// fakeRetrievalIndex allows inspection of the 'k' value passed to Search.
type fakeRetrievalIndex struct {
	lastK int
}

func (f *fakeRetrievalIndex) Add(label uint64, vector []float32) error { return nil }
func (f *fakeRetrievalIndex) Search(vector []float32, k int) ([]uint64, []float32, error) {
	f.lastK = k
	return []uint64{}, []float32{}, nil
}
func (f *fakeRetrievalIndex) Save(path string) error { return nil }
func (f *fakeRetrievalIndex) Load(path string) error { return nil }
func (f *fakeRetrievalIndex) Close() error           { return nil }

func (e *fakeRetrievalEmbedder) Embed(_ context.Context, model string, texts []string) ([][]float32, error) {
	// return one embedding per input text, matching the real embedder behaviour
	n := len(texts)
	if n == 0 {
		return [][]float32{}, nil
	}
	var vec []float32
	if v, ok := e.vectorsByModel[model]; ok {
		vec = v
	} else {
		vec = []float32{1, 0}
	}
	res := make([][]float32, n)
	for i := range res {
		res[i] = vec
	}
	return res, nil
}

func TestAsk_GeneratorErrorLogged(t *testing.T) {
	buf := &bytes.Buffer{}

	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	longQ := strings.Repeat("x", 100)
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, &fakeGenerator{out: "unused", err: errors.New("oh no")})
	svc.SetLogger(log.New(buf, "", 0))
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "doc.txt", DocType: "md", Snippet: "hi"})

	res, err := svc.Ask(context.Background(), longQ, model.SearchQuery{Query: "", K: 1})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if res.Answer == "" || !strings.Contains(res.Answer, longQ[:10]) {
		t.Fatalf("unexpected answer: %q", res.Answer)
	}
	logOutput := buf.String()
	if !strings.Contains(logOutput, "generator error") {
		t.Fatalf("expected log entry about generator error, got %q", logOutput)
	}
	if strings.Contains(logOutput, longQ) {
		t.Fatalf("log output should not contain full question, got %q", logOutput)
	}
}

func TestAsk_IndexingCompleteFlag(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	// provider returns false to simulate ongoing indexing
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)
	svc.SetIndexingCompleteProvider(func() bool { return false })
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "doc.txt", DocType: "md", Snippet: "hi"})

	res, err := svc.Ask(context.Background(), "question", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if res.IndexingComplete != false {
		t.Fatalf("expected indexing complete false, got %v", res.IndexingComplete)
	}
}

func TestAsk_IndexingCompleteDefaultTrue(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)
	// no provider set, should default to true
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "doc.txt", DocType: "md", Snippet: "hi"})

	res, err := svc.Ask(context.Background(), "question", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if !res.IndexingComplete {
		t.Fatalf("expected indexing complete true with nil provider, got %v", res.IndexingComplete)
	}
}

// Test the IndexingComplete accessor directly; the Ask tests above exercised it
// indirectly, but having a standalone verification improves clarity and
// prevents regressions when the interface changes.
func TestIndexingCompleteAccessorDirect(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)

	// default provider nil -> should report true
	if ic, err := svc.IndexingComplete(context.Background()); err != nil || !ic {
		t.Fatalf("expected true default, got %v,%v", ic, err)
	}

	svc.SetIndexingCompleteProvider(func() bool { return false })
	if ic, err := svc.IndexingComplete(context.Background()); err != nil || ic {
		t.Fatalf("expected false after setting provider, got %v,%v", ic, err)
	}
}

func TestIndexingComplete_CanceledContext(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)
	// provider returns true but should not be invoked
	svc.SetIndexingCompleteProvider(func() bool { t.Fatal("provider should not be called"); return true })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, err := svc.IndexingComplete(ctx)
	if err == nil {
		t.Fatalf("expected error when context canceled")
	}
	if ok {
		t.Fatalf("expected false result when context canceled")
	}
}

func TestSearch_ReturnsRankedHitsWithFilters(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	if err := idx.Add(2, []float32{0.9, 0.1}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	if err := idx.Add(3, []float32{0, 1}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {0, 1},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "docs/a.md", DocType: "md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "src/main.go", DocType: "code", Snippet: "beta"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "docs/b.md", DocType: "md", Snippet: "gamma"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query:      "alpha",
		K:          2,
		PathPrefix: "docs/",
		FileGlob:   "docs/a.*",
		DocTypes:   []string{"md"},
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 filtered hit, got %d", len(hits))
	}
	if hits[0].RelPath != "docs/a.md" {
		t.Fatalf("unexpected top hit: %#v", hits[0])
	}
}

func TestSearch_FileGlobFilter(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(10, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	if err := idx.Add(20, []float32{0.8, 0.2}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {0, 1},
	}}, nil)
	svc.SetChunkMetadata(10, model.SearchHit{RelPath: "src/a.go", DocType: "code"})
	svc.SetChunkMetadata(20, model.SearchHit{RelPath: "docs/a.md", DocType: "md"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query:    "q",
		K:        5,
		FileGlob: "src/*.go",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].RelPath != "src/a.go" {
		t.Fatalf("unexpected glob filtered hits: %#v", hits)
	}
}

func TestSearch_OverfetchMultiplier_DefaultAndConfigurable(t *testing.T) {
	fi := &fakeRetrievalIndex{}
	svc := NewService(nil, fi, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{}}, nil)
	// first search should use default multiplier (5)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 3}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 3*5 {
		t.Fatalf("expected default overfetch 5x (got %d)", fi.lastK)
	}
	// change multiplier to a smaller value and verify it takes effect
	svc.SetOverfetchMultiplier(2)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 3}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 3*2 {
		t.Fatalf("expected overfetch 2x after set (got %d)", fi.lastK)
	}
	// invalid values should be normalized
	svc.SetOverfetchMultiplier(0)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 1}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 1*1 {
		t.Fatalf("multiplier lower bound not enforced (got %d)", fi.lastK)
	}
	// extremely large value should be capped
	svc.SetOverfetchMultiplier(1000)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 1}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK > 100*1 {
		t.Fatalf("multiplier upper cap not enforced (got %d)", fi.lastK)
	}
}

// ensure we don't overflow when k * multiplier would exceed the range of
// int.  The actual index implementation can't handle a number this large
// anyway, so we expect the result to be clamped to math.MaxInt.
func TestSearch_OverflowProtection(t *testing.T) {
	fi := &fakeRetrievalIndex{}
	svc := NewService(nil, fi, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{}}, nil)
	// set multiplier to max allowed by the setter; the service doesn't crash
	svc.SetOverfetchMultiplier(100)
	// choose a k that's guaranteed to overflow when multiplied by 100
	bigK := math.MaxInt/100 + 1
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: bigK}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != math.MaxInt {
		t.Fatalf("expected clamped value math.MaxInt (%d), got %d", math.MaxInt, fi.lastK)
	}
}

func TestSearch_BothMode_DedupesAndNormalizes(t *testing.T) {
	textIdx := index.NewHNSWIndex("")
	codeIdx := index.NewHNSWIndex("")
	if err := textIdx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("textIdx.Add failed: %v", err)
	}
	if err := textIdx.Add(2, []float32{0.8, 0.2}); err != nil {
		t.Fatalf("textIdx.Add failed: %v", err)
	}
	if err := codeIdx.Add(2, []float32{0.9, 0.1}); err != nil { // duplicate label across indexes
		t.Fatalf("codeIdx.Add failed: %v", err)
	}
	if err := codeIdx.Add(3, []float32{1, 0}); err != nil {
		t.Fatalf("codeIdx.Add failed: %v", err)
	}

	svc := NewService(nil, textIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {1, 0},
	}}, nil)
	svc.SetCodeIndex(codeIdx)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "docs/1.md", DocType: "md"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "src/2.go", DocType: "code"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "src/3.go", DocType: "code"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "query",
		K:     10,
		Index: "both",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 deduped hits, got %d", len(hits))
	}
	seen := map[uint64]bool{}
	for _, hit := range hits {
		if seen[hit.ChunkID] {
			t.Fatalf("duplicate chunk in merged results: %d", hit.ChunkID)
		}
		seen[hit.ChunkID] = true
		if hit.Score < 0 || hit.Score > 1 {
			t.Fatalf("score should be normalized to [0,1], got %f", hit.Score)
		}
	}
}

func TestOpenFile_LineSpan(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "docs", "a.md")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("one\ntwo\nthree\nfour"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	out, err := svc.OpenFile(context.Background(), "docs/a.md", model.Span{Kind: "lines", StartLine: 2, EndLine: 3}, 200)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if out != "two\nthree" {
		t.Fatalf("unexpected open_file slice: %q", out)
	}
}

func TestOpenFile_PathExcluded(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "private", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("super secret"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetPathExcludes([]string{"**/private/**"})
	_, err := svc.OpenFile(context.Background(), "private/secret.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestOpenFile_ContentSecretBlocked(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "docs", "token.txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("Authorization: Bearer abcdefgh.ijklmnop.qrstuvwx"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	_, err := svc.OpenFile(context.Background(), "docs/token.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestOpenFile_PathTraversalBlocked(t *testing.T) {
	root := t.TempDir()
	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	_, err := svc.OpenFile(context.Background(), "../outside.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrPathOutsideRoot) {
		t.Fatalf("expected ErrPathOutsideRoot, got %v", err)
	}
}

func TestOpenFile_PageSpan(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "docs", "ocr.txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("page1\fpage2\fpage3"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	out, err := svc.OpenFile(context.Background(), "docs/ocr.txt", model.Span{Kind: "page", Page: 2}, 200)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if out != "page2" {
		t.Fatalf("unexpected page slice: %q", out)
	}
}

func TestOpenFile_TimeSpan(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "audio", "transcript.txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	content := "[00:00] intro\n[00:02] alpha\n[00:05] beta\n[00:10] omega"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	out, err := svc.OpenFile(context.Background(), "audio/transcript.txt", model.Span{Kind: "time", StartMS: 2000, EndMS: 6000}, 200)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	want := "[00:02] alpha\n[00:05] beta"
	if out != want {
		t.Fatalf("unexpected time slice: got %q want %q", out, want)
	}
}

func TestMatchExcludePattern_Concurrent(t *testing.T) {
	svc := NewService(nil, nil, nil, nil)
	pattern := "**/foo/**"
	const goroutines = 20

	for i := 0; i < goroutines; i++ {
		i := i // capture for closure
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Parallel()
			if !svc.matchExcludePattern(pattern, "a/foo/b") {
				t.Error("expected pattern to match")
			}
		})
	}
}

func TestOpenFile_PageSpan_FromMetadata(t *testing.T) {
	root := t.TempDir()
	// Keep the path inside root but do not create file contents; metadata should drive output.
	path := filepath.Join(root, "docs", "ocr.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/ocr.txt",
		Snippet: "page-two-snippet",
		Span:    model.Span{Kind: "page", Page: 2},
	})
	out, err := svc.OpenFile(context.Background(), "docs/ocr.txt", model.Span{Kind: "page", Page: 2}, 200)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if out != "page-two-snippet" {
		t.Fatalf("unexpected metadata page slice: %q", out)
	}
}

func TestOpenFile_TimeSpan_FromMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "audio", "transcript.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	svc := NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "audio/transcript.txt",
		Snippet: "alpha",
		Span:    model.Span{Kind: "time", StartMS: 1000, EndMS: 3000},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "audio/transcript.txt",
		Snippet: "beta",
		Span:    model.Span{Kind: "time", StartMS: 4000, EndMS: 6000},
	})
	out, err := svc.OpenFile(context.Background(), "audio/transcript.txt", model.Span{Kind: "time", StartMS: 2000, EndMS: 5000}, 200)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if out != "alpha\nbeta" {
		t.Fatalf("unexpected metadata time slice: %q", out)
	}
}

func TestLooksLikeCodeQuery(t *testing.T) {
	cases := []struct {
		query  string
		expect bool
	}{
		{"func main() {}", true},
		{"I love python", false},
		{"go to the store", false},
		{"use import and ([])", true},
		{"I wrote code in .go file", false},
		{"I wrote code in .go file `snippet`", true},
		{"this is just plain text", false},
		{"```go\nfmt.Println(\"hi\")\n```", true},
		{"some `inline code`", false},
		{"python code", false},
		{"fix bug in java { }", false},
	}

	for _, c := range cases {
		got := looksLikeCodeQuery(c.query)
		if got != c.expect {
			t.Errorf("looksLikeCodeQuery(%q) = %v; want %v", c.query, got, c.expect)
		}
	}
}

func TestAsk_FallbackAndCitations(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 10, EndLine: 12},
	})

	got, err := svc.Ask(context.Background(), "what is alpha?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if got.Question != "what is alpha?" {
		t.Fatalf("unexpected question in result: %q", got.Question)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(got.Hits))
	}
	if len(got.Citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(got.Citations))
	}
	if got.Citations[0].RelPath != "docs/a.md" {
		t.Fatalf("unexpected citation path: %q", got.Citations[0].RelPath)
	}
	if got.Answer == "" || !strings.Contains(got.Answer, "alpha snippet") {
		t.Fatalf("expected fallback answer to include snippet, got %q", got.Answer)
	}
}

func TestAsk_UsesGeneratorWhenAvailable(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "Generated answer with [docs/a.md]"})
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if got.Answer != "Generated answer with [docs/a.md]" {
		t.Fatalf("expected generated answer, got %q", got.Answer)
	}
}

func TestAsk_AppendsMissingAttributions(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	if err := idx.Add(2, []float32{0.9, 0.1}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "Generated summary without explicit source tags."})
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "docs/b.md",
		Snippet: "beta snippet",
		Span:    model.Span{Kind: "lines", StartLine: 3, EndLine: 4},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 2})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(got.Answer, "Sources:") {
		t.Fatalf("expected answer to include sources suffix, got %q", got.Answer)
	}
	if !strings.Contains(got.Answer, "[docs/a.md]") || !strings.Contains(got.Answer, "[docs/b.md]") {
		t.Fatalf("expected answer to include missing source tags, got %q", got.Answer)
	}
}

func TestAsk_AppendsOnlyMissingAttributions(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	if err := idx.Add(2, []float32{0.9, 0.1}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "Generated summary with [docs/a.md] already present."})
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "docs/b.md",
		Snippet: "beta snippet",
		Span:    model.Span{Kind: "lines", StartLine: 3, EndLine: 4},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 2})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	// should still include both a.md and b.md, but a.md only once
	if !strings.Contains(got.Answer, "[docs/a.md]") {
		t.Fatalf("expected answer to still contain [docs/a.md], got %q", got.Answer)
	}
	if !strings.Contains(got.Answer, "[docs/b.md]") {
		t.Fatalf("expected answer to contain [docs/b.md], got %q", got.Answer)
	}
	if strings.Count(got.Answer, "[docs/a.md]") != 1 {
		t.Fatalf("expected only one [docs/a.md] tag, got %q", got.Answer)
	}
}
