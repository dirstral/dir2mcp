package tests

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dir2mcp/internal/index"
	"dir2mcp/internal/model"
	"dir2mcp/internal/retrieval"
)

type fakeRetrievalEmbedder struct {
	vectorsByModel map[string][]float32
}

type fakeGenerator struct {
	out        string
	err        error
	lastPrompt string
}

func (g *fakeGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.lastPrompt = prompt
	if g.err != nil {
		return "", g.err
	}
	return g.out, nil
}

// paginateDocs returns a slice of documents based on the provided limit/offset
// along with the total number of documents.  It makes a copy of the returned slice
// so callers can mutate safely.  Logic is intentionally identical to the real store
// implementations exercised elsewhere in tests.
func paginateDocs(docs []model.Document, limit, offset int) ([]model.Document, int64) {
	// mirror store pagination logic; callers in tests rely on this behavior.  We
	// defensively normalise inputs to avoid panics when a test accidentally
	// passes a negative value.  Negative offsets clamp to 0 and negative limits
	// behave like 0, resulting in empty slices.
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	total := int64(len(docs))
	if offset >= len(docs) {
		return []model.Document{}, total
	}
	end := offset + limit
	if end > len(docs) {
		end = len(docs)
	}
	return append([]model.Document(nil), docs[offset:end]...), total
}

// simple regression checks against the defensive behaviour added above.
func TestPaginateDocs_NegativeInputs(t *testing.T) {
	docs := []model.Document{{RelPath: "a"}, {RelPath: "b"}}
	out, total := paginateDocs(docs, -5, -3)
	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
	if len(out) != 0 {
		t.Errorf("expected empty result for negative limit/offset, got %v", out)
	}

	out, _ = paginateDocs(docs, 1, -1)
	if len(out) != 1 || out[0].RelPath != "a" {
		t.Errorf("unexpected result for offset -1: %v", out)
	}

	out, _ = paginateDocs(docs, -1, 1)
	if len(out) != 0 {
		t.Errorf("expected empty slice for negative limit, got %v", out)
	}

	out, _ = paginateDocs(docs, 1, 5)
	if len(out) != 0 {
		t.Errorf("offset beyond len should return empty slice, got %v", out)
	}
}

type fakeStatsStore struct {
	docs      []model.Document
	corpus    model.CorpusStats
	corpusErr error
}

func (f *fakeStatsStore) Init(context.Context) error { return nil }
func (f *fakeStatsStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (f *fakeStatsStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}
func (f *fakeStatsStore) ListFiles(_ context.Context, _ string, _ string, limit, offset int) ([]model.Document, int64, error) {
	docs, total := paginateDocs(f.docs, limit, offset)
	return docs, total, nil
}
func (f *fakeStatsStore) Close() error { return nil }

func (f *fakeStatsStore) CorpusStats(_ context.Context) (model.CorpusStats, error) {
	if f.corpusErr != nil {
		return model.CorpusStats{}, f.corpusErr
	}
	copyCounts := make(map[string]int64, len(f.corpus.DocCounts))
	for k, v := range f.corpus.DocCounts {
		copyCounts[k] = v
	}
	out := f.corpus
	out.DocCounts = copyCounts
	return out, nil
}

type fakeListOnlyStore struct {
	docs []model.Document
}

func (f *fakeListOnlyStore) Init(context.Context) error { return nil }
func (f *fakeListOnlyStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (f *fakeListOnlyStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}
func (f *fakeListOnlyStore) ListFiles(_ context.Context, _ string, _ string, limit, offset int) ([]model.Document, int64, error) {
	docs, total := paginateDocs(f.docs, limit, offset)
	return docs, total, nil
}
func (f *fakeListOnlyStore) Close() error { return nil }

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
		clone := make([]float32, len(vec))
		copy(clone, vec)
		res[i] = clone
	}
	return res, nil
}

func TestAsk_GeneratorErrorLogged(t *testing.T) {
	buf := &bytes.Buffer{}

	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, &fakeGenerator{out: "unused", err: errors.New("oh no")})
	svc.SetLogger(log.New(buf, "", 0))
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "doc.txt", DocType: "md", Snippet: "hi"})

	res, err := svc.Ask(context.Background(), "question", model.SearchQuery{Query: "", K: 1})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	// fallback behavior: generator failed, so the service should return
	// something derived from the chunk metadata we previously stored. the
	// previous check looked for the literal word "question", which is brittle
	// and not guaranteed. instead we assert that the answer contains the
	// snippet we seeded above ("hi"), while still guarding against an empty
	// answer.
	if res.Answer == "" || !strings.Contains(res.Answer, "hi") {
		t.Fatalf("unexpected answer: %q", res.Answer)
	}
	if !strings.Contains(buf.String(), "generator error") {
		t.Fatalf("expected log entry about generator error, got %q", buf.String())
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

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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
	svc := retrieval.NewService(nil, fi, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{}}, nil)
	// first search should use default multiplier (5)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 3}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 3*5 {
		t.Fatalf("expected default overfetch 5x (got %d)", fi.lastK)
	}
	// change multiplier to a smaller value and verify it takes effect
	svc.SetOversampleFactor(2)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 3}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 3*2 {
		t.Fatalf("expected overfetch 2x after set (got %d)", fi.lastK)
	}
	// invalid values should be normalized
	svc.SetOversampleFactor(0)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 1}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 1*1 {
		t.Fatalf("multiplier lower bound not enforced (got %d)", fi.lastK)
	}
	// extremely large value should be capped
	svc.SetOversampleFactor(1000)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 1}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK > 100*1 {
		t.Fatalf("multiplier upper cap not enforced (got %d)", fi.lastK)
	}

	// backward-compatible setter should still work.
	svc.SetOverfetchMultiplier(3)
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 2}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if fi.lastK != 2*3 {
		t.Fatalf("expected overfetch 3x after legacy setter (got %d)", fi.lastK)
	}
}

// ensure we don't overflow when k * multiplier would exceed the range of
// int.  The actual index implementation can't handle a number this large
// anyway, so we expect the result to be clamped to math.MaxInt.
func TestSearch_OverflowProtection(t *testing.T) {
	fi := &fakeRetrievalIndex{}
	svc := retrieval.NewService(nil, fi, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{}}, nil)
	// set multiplier to max allowed by the setter; the service doesn't crash
	svc.SetOversampleFactor(100)
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

	svc := retrieval.NewService(nil, textIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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

	svc := retrieval.NewService(nil, nil, nil, nil)
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

	svc := retrieval.NewService(nil, nil, nil, nil)
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

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	_, err := svc.OpenFile(context.Background(), "docs/token.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestOpenFile_PathTraversalBlocked(t *testing.T) {
	root := t.TempDir()
	svc := retrieval.NewService(nil, nil, nil, nil)
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

	svc := retrieval.NewService(nil, nil, nil, nil)
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

	svc := retrieval.NewService(nil, nil, nil, nil)
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
	svc := retrieval.NewService(nil, nil, nil, nil)
	pattern := "**/foo/**"
	var wg sync.WaitGroup
	const goroutines = 20
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if !svc.MatchExcludePattern(pattern, "a/foo/b") {
				t.Error("expected pattern to match")
			}
		}()
	}
	wg.Wait()
}

func TestOpenFile_PageSpan_FromMetadata(t *testing.T) {
	root := t.TempDir()
	// Keep the path inside root but do not create file contents; metadata should drive output.
	path := filepath.Join(root, "docs", "ocr.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
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

	svc := retrieval.NewService(nil, nil, nil, nil)
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
		got := retrieval.LooksLikeCodeQuery(c.query)
		if got != c.expect {
			t.Errorf("retrieval.LooksLikeCodeQuery(%q) = %v; want %v", c.query, got, c.expect)
		}
	}
}

func TestAsk_FallbackAndCitations(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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

func TestAsk_EmptyContext(t *testing.T) {
	svc := retrieval.NewService(nil, index.NewHNSWIndex(""), &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)

	got, err := svc.Ask(context.Background(), "question", model.SearchQuery{K: 3})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if got.Answer != "No relevant context found in the indexed corpus." {
		t.Fatalf("unexpected empty-context answer: %q", got.Answer)
	}
	if len(got.Hits) != 0 {
		t.Fatalf("expected no hits, got %#v", got.Hits)
	}
	if len(got.Citations) != 0 {
		t.Fatalf("expected no citations, got %#v", got.Citations)
	}
}

func TestStats_UsesCorpusStats(t *testing.T) {
	st := &fakeStatsStore{
		corpus: model.CorpusStats{
			DocCounts:       map[string]int64{"code": 2, "md": 1},
			TotalDocs:       3,
			Scanned:         4,
			Indexed:         2,
			Skipped:         1,
			Deleted:         1,
			Representations: 6,
			ChunksTotal:     8,
			EmbeddedOK:      7,
			Errors:          1,
		},
	}

	svc := retrieval.NewService(st, nil, nil, nil)
	svc.SetRootDir("/repo")
	svc.SetStateDir("/repo/.dir2mcp")
	svc.SetProtocolVersion("2025-11-25")

	got, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if got.Root != "/repo" || got.StateDir != "/repo/.dir2mcp" || got.ProtocolVersion != "2025-11-25" {
		t.Fatalf("unexpected metadata fields: %+v", got)
	}
	if got.TotalDocs != 3 || got.DocCounts["code"] != 2 || got.DocCounts["md"] != 1 {
		t.Fatalf("unexpected doc counts in stats: %+v", got)
	}
	if got.Scanned != 4 || got.Indexed != 2 || got.Skipped != 1 || got.Deleted != 1 {
		t.Fatalf("unexpected lifecycle counts in stats: %+v", got)
	}
	if got.Representations != 6 || got.ChunksTotal != 8 || got.EmbeddedOK != 7 || got.Errors != 1 {
		t.Fatalf("unexpected chunk counters in stats: %+v", got)
	}
}

func TestStats_FallbackFromListFiles(t *testing.T) {
	st := &fakeListOnlyStore{
		docs: []model.Document{
			{RelPath: "src/a.go", DocType: "code", Status: "ok"},
			{RelPath: "docs/readme.md", DocType: "md", Status: "skipped"},
			{RelPath: "docs/error.md", DocType: "md", Status: "error"},
			{RelPath: "old/deleted.md", DocType: "md", Status: "ok", Deleted: true},
		},
	}

	svc := retrieval.NewService(st, nil, nil, nil)
	got, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if got.Scanned != 4 || got.Indexed != 1 || got.Skipped != 1 || got.Deleted != 1 || got.Errors != 1 {
		t.Fatalf("unexpected fallback lifecycle counts: %+v", got)
	}
	if got.TotalDocs != 3 || got.DocCounts["code"] != 1 || got.DocCounts["md"] != 2 {
		t.Fatalf("unexpected fallback doc counts: %+v", got)
	}
	if got.Representations != 0 || got.ChunksTotal != 0 || got.EmbeddedOK != 0 {
		t.Fatalf("expected zero chunk counters in fallback path: %+v", got)
	}
}

func TestAsk_UsesGeneratorWhenAvailable(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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

func TestAsk_UsesConfiguredSystemPromptAndContextBudget(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	gen := &fakeGenerator{out: "ok [docs/a.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: strings.Repeat("alpha ", 80),
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetRAGSystemPrompt("Custom system prompt")
	svc.SetMaxContextChars(40)

	if _, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(gen.lastPrompt, "Custom system prompt") {
		t.Fatalf("expected custom system prompt, got %q", gen.lastPrompt)
	}
	if strings.Contains(gen.lastPrompt, "Answer the question using only the provided context.") {
		t.Fatalf("expected default system prompt to be replaced, got %q", gen.lastPrompt)
	}
	parts := strings.SplitN(gen.lastPrompt, "\n\nContext:\n", 2)
	if len(parts) != 2 {
		t.Fatalf("expected Context section in prompt, got %q", gen.lastPrompt)
	}
	if got := len([]rune(parts[1])); got > 40 {
		t.Fatalf("context budget exceeded: got %d chars, want <= 40", got)
	}
}

func TestAsk_DefaultSystemPromptCompatibility(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("idx.Add failed: %v", err)
	}

	gen := &fakeGenerator{out: "ok [docs/a.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "docs/a.md", Snippet: "alpha"})

	if _, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(gen.lastPrompt, "Answer the question using only the provided context.") {
		t.Fatalf("expected backward-compatible default prompt, got %q", gen.lastPrompt)
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
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
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
