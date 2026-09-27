package tests

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/latechunk"
	"github.com/dirstral/dir2mcp/internal/model"
)

// lcPlainEmbedder implements only model.Embedder (one pooled vector per input),
// like every hosted provider: it can never serve the late-chunking path.
type lcPlainEmbedder struct{}

func (lcPlainEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1}
	}
	return out, nil
}

// lcTokenEmbedder additionally implements model.TokenEmbedder, so it can serve
// the late-chunking path.
type lcTokenEmbedder struct{ lcPlainEmbedder }

func (lcTokenEmbedder) EmbedDocumentTokens(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([]model.TokenEmbedding, error) {
	out := make([]model.TokenEmbedding, len(inputs))
	for i := range inputs {
		out[i] = model.TokenEmbedding{Vectors: [][]float32{{1}}, Offsets: []int{0}, Ends: []int{1}}
	}
	return out, nil
}

// TestWorker_LateChunkDecision_DisabledByDefault asserts a worker with the
// feature off (the default) reports an inactive, disabled decision regardless of
// embedder capability — so behavior is unchanged unless explicitly enabled.
func TestWorker_LateChunkDecision_DisabledByDefault(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcTokenEmbedder{}}
	dec := w.LateChunkDecision()
	if dec.Active {
		t.Fatal("late chunking must be inactive by default")
	}
	if dec.Fallback != latechunk.FallbackDisabled {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackDisabled)
	}
}

// TestWorker_LateChunkDecision_FallsBackWithoutTokenEmbedder asserts that
// enabling the feature with a plain embedder (every hosted provider) gracefully
// falls back to chunk-then-embed rather than activating.
func TestWorker_LateChunkDecision_FallsBackWithoutTokenEmbedder(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcPlainEmbedder{}, LateChunking: true}
	dec := w.LateChunkDecision()
	if dec.Active {
		t.Fatal("late chunking must fall back when the embedder lacks token embeddings")
	}
	if dec.Fallback != latechunk.FallbackNoTokenEmbedder {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackNoTokenEmbedder)
	}
}

// TestWorker_LateChunkDecision_ActiveWithTokenEmbedder asserts the path
// activates when enabled and the configured embedder exposes token embeddings.
func TestWorker_LateChunkDecision_ActiveWithTokenEmbedder(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcTokenEmbedder{}, LateChunking: true}
	dec := w.LateChunkDecision()
	if !dec.Active {
		t.Fatal("late chunking should activate with a token embedder when enabled")
	}
	if dec.Embedder == nil {
		t.Fatal("active decision must carry a non-nil token embedder")
	}
}

// lcWordEmbedder implements model.Embedder and model.TokenEmbedder with a
// deterministic, checkable geometry: EmbedDocumentTokens tokenizes the document
// on ASCII spaces (rune offsets) and gives word i the vector [i+1, 1]; Embed
// returns [0, 1] for every input. It records what the worker drove so a test can
// prove which path ran. tokenErr, when set, is returned from EmbedDocumentTokens.
type lcWordEmbedder struct {
	embedCalls  int
	embedInputs []string
	tokenCalls  int
	tokenDocs   []string
	tokenErr    error
	// failRepText, when set, makes EmbedDocumentTokens fail non-transiently for
	// exactly that document text, so a test can fail ONE representation of a
	// batch and watch its siblings still pool.
	failRepText string
}

func (e *lcWordEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.embedCalls++
	e.embedInputs = append(e.embedInputs, inputs...)
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{0, 1}
	}
	return out, nil
}

func (e *lcWordEmbedder) EmbedDocumentTokens(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([]model.TokenEmbedding, error) {
	e.tokenCalls++
	e.tokenDocs = append(e.tokenDocs, inputs...)
	if e.tokenErr != nil {
		return nil, e.tokenErr
	}
	for _, doc := range inputs {
		if e.failRepText != "" && doc == e.failRepText {
			return nil, &model.ProviderError{Code: "TEI_FAILED", Message: "Validation: rejected", Retryable: false, StatusCode: http.StatusUnprocessableEntity}
		}
	}
	out := make([]model.TokenEmbedding, len(inputs))
	for i, doc := range inputs {
		out[i] = wordTokens(doc)
	}
	return out, nil
}

// wordTokens is the fake's tokenizer: one token per space-separated word, rune
// offsets, vector [wordIndex+1, 1].
func wordTokens(doc string) model.TokenEmbedding {
	var te model.TokenEmbedding
	runes := []rune(doc)
	start := -1
	word := 0
	for i := 0; i <= len(runes); i++ {
		if i == len(runes) || runes[i] == ' ' {
			if start >= 0 {
				te.Vectors = append(te.Vectors, []float32{float32(word + 1), 1})
				te.Offsets = append(te.Offsets, start)
				te.Ends = append(te.Ends, i)
				word++
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return te
}

// lcTextSource is a chunk source that also serves representation text
// (model.RepresentationTextReader), as the SQLite store does.
type lcTextSource struct {
	fakeChunkSource
	texts map[int64]string
}

func (s *lcTextSource) RepresentationText(_ context.Context, repID int64) (string, bool, error) {
	text, ok := s.texts[repID]
	return text, ok, nil
}

// capturingIndex records every upserted vector by chunk id so a test can check
// the pooled geometry, not just that something was indexed.
type capturingIndex struct {
	model.Index
	vectors map[uint64][]float32
}

func newCapturingIndex() *capturingIndex {
	return &capturingIndex{Index: index.NewHNSWIndex(""), vectors: make(map[uint64][]float32)}
}

func (c *capturingIndex) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	cp := make([]float32, len(vector))
	copy(cp, vector)
	c.vectors[payload.ChunkID] = cp
	return c.Index.Upsert(ctx, vector, payload)
}

// lcTask builds a pending text chunk task with its representation and rune span.
func lcTask(label uint64, repID int64, text string, runeStart, runeEnd int) model.ChunkTask {
	tk := model.NewChunkTask(label, text, "text", model.ChunkMetadata{ChunkID: label, RelPath: "a.txt", DocType: "text"})
	tk.RepID = repID
	tk.RuneStart = runeStart
	tk.RuneEnd = runeEnd
	return tk
}

func lcWorker(src index.ChunkSource, ix model.Index, emb model.Embedder, buf *bytes.Buffer) *index.EmbeddingWorker {
	return &index.EmbeddingWorker{
		Source: src, Index: ix, Embedder: emb,
		BatchSize: 8, LateChunking: true, Logger: log.New(buf, "", 0),
	}
}

func unit(v ...float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func vecClose(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 1e-5 {
			return false
		}
	}
	return true
}

// TestWorker_LateChunkActive_PoolsWholeDocument pins the wired path (issues
// #446 F1 / #565, SPEC 8.1.9): with the mode on and a token embedder, the worker
// embeds the representation's document text ONCE through EmbedDocumentTokens,
// never calls Embed, and indexes each chunk as the L2-normalized mean of the
// token vectors inside its rune span. The one-time log now earns "active".
func TestWorker_LateChunkActive_PoolsWholeDocument(t *testing.T) {
	const doc = "alpha beta gamma delta" // words 0..3 -> vectors [1,1] [2,1] [3,1] [4,1]
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "gamma delta", 11, 22),
		}},
		texts: map[int64]string{7: doc},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	n, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2", n)
	}
	if emb.tokenCalls != 1 || len(emb.tokenDocs) != 1 || emb.tokenDocs[0] != doc {
		t.Fatalf("the document must be token-embedded exactly once: calls=%d docs=%q", emb.tokenCalls, emb.tokenDocs)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("chunk-then-embed must not run when every chunk pooled: embedCalls=%d inputs=%q", emb.embedCalls, emb.embedInputs)
	}
	// chunk 1 = mean([1,1],[2,1]) = [1.5,1]; chunk 2 = mean([3,1],[4,1]) = [3.5,1]; both unit-norm.
	if got, want := ix.vectors[1], unit(1.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 1 vector = %v, want %v (normalized mean of its tokens)", got, want)
	}
	if got, want := ix.vectors[2], unit(3.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 2 vector = %v, want %v (normalized mean of its tokens)", got, want)
	}
	if len(src.embedded) != 2 || len(src.failedLabels) != 0 {
		t.Fatalf("embedded=%v failed=%v, want both embedded and none failed", src.embedded, src.failedLabels)
	}
	logged := buf.String()
	if !strings.Contains(logged, "enabled and active") {
		t.Fatalf("the one-time log must report the mode as active now that the path runs: %q", logged)
	}
	if strings.Contains(logged, "not yet wired") {
		t.Fatalf("the deferred-state log must be gone: %q", logged)
	}
}

// TestWorker_LateChunkActive_StraddlingTokenPoolsIntoBothChunks pins the
// half-open overlap rule end to end: a token that straddles a chunk boundary
// contributes to both neighbouring chunks (SPEC 8.1.9 "Pooling").
func TestWorker_LateChunkActive_StraddlingTokenPoolsIntoBothChunks(t *testing.T) {
	const doc = "aa bb cc" // tokens [0,2)->[1,1] [3,5)->[2,1] [6,8)->[3,1]
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 3, "aa b", 0, 4), // overlaps tokens 0 and 1
			lcTask(2, 3, "b cc", 4, 8), // overlaps tokens 1 and 2
		}},
		texts: map[int64]string{3: doc},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	if _, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got, want := ix.vectors[1], unit(1.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 1 = %v, want %v", got, want)
	}
	if got, want := ix.vectors[2], unit(2.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 2 = %v, want %v", got, want)
	}
}

// TestWorker_LateChunkActive_UnpoolableChunkFailsWithReindexRemediation pins
// SPEC 8.1.9 "Pre-feature rows": under an active token embedder a text chunk
// with no rune span is NOT embedded chunk-then-embed (that would put an unpooled
// vector into a pooled corpus); it is marked failed with a reason that names
// `dir2mcp reindex`, while its poolable sibling still pools and indexes.
func TestWorker_LateChunkActive_UnpoolableChunkFailsWithReindexRemediation(t *testing.T) {
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "gamma delta", -1, -1), // pre-feature row: no span
		}},
		texts: map[int64]string{7: "alpha beta gamma delta"},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	n, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1 (only the poolable chunk)", n)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("an unpoolable chunk must never be embedded chunk-then-embed: embedInputs=%q", emb.embedInputs)
	}
	if len(src.failedLabels) != 1 || src.failedLabels[0] != 2 {
		t.Fatalf("failed labels = %v, want [2]", src.failedLabels)
	}
	if !strings.Contains(src.failedReason, "dir2mcp reindex") {
		t.Fatalf("failure reason must name the remediation: %q", src.failedReason)
	}
	if src.failedCategory != "embedding_failure" {
		t.Fatalf("failed category = %q, want embedding_failure", src.failedCategory)
	}
	if _, indexed := ix.vectors[2]; indexed {
		t.Fatal("the unpoolable chunk must not reach the index")
	}
	if len(src.embedded) != 1 || src.embedded[0] != 1 {
		t.Fatalf("embedded = %v, want [1]", src.embedded)
	}
}

// TestWorker_LateChunkActive_MissingDocumentTextFails pins the other half of
// "Pre-feature rows": a chunk whose representation has no persisted document
// text is failed with the reindex remediation, and no token or plain embed call
// is made for it.
func TestWorker_LateChunkActive_MissingDocumentTextFails(t *testing.T) {
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{lcTask(1, 9, "alpha beta", 0, 10)}},
		texts:           map[int64]string{}, // rep 9 has no text
	}
	var buf bytes.Buffer
	n, err := lcWorker(src, newCapturingIndex(), emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || emb.tokenCalls != 0 || emb.embedCalls != 0 {
		t.Fatalf("n=%d tokenCalls=%d embedCalls=%d, want 0/0/0", n, emb.tokenCalls, emb.embedCalls)
	}
	if len(src.failedLabels) != 1 || !strings.Contains(src.failedReason, "dir2mcp reindex") {
		t.Fatalf("failed=%v reason=%q, want [1] with a reindex remediation", src.failedLabels, src.failedReason)
	}
}

// TestWorker_LateChunkActive_TransientTokenErrorLeavesPending pins SPEC 8.1.9
// "Failure classification": a transient token-embedding failure (a 429) leaves
// the chunks PENDING for a later cycle and MUST NOT degrade into
// chunk-then-embed vectors inside a pooled corpus.
func TestWorker_LateChunkActive_TransientTokenErrorLeavesPending(t *testing.T) {
	emb := &lcWordEmbedder{tokenErr: &model.ProviderError{Code: "TEI_RATE_LIMIT", Message: "overloaded", Retryable: true, StatusCode: http.StatusTooManyRequests}}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "gamma delta", 11, 22),
		}},
		texts: map[int64]string{7: "alpha beta gamma delta"},
	}
	var buf bytes.Buffer
	n, err := lcWorker(src, newCapturingIndex(), emb, &buf).RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("a transient token-embed failure must surface as an error so the run loop backs off")
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("a transient failure must not fall back to chunk-then-embed: embedInputs=%q", emb.embedInputs)
	}
	if len(src.embedded) != 0 || len(src.failedLabels) != 0 {
		t.Fatalf("chunks must stay pending: embedded=%v failed=%v", src.embedded, src.failedLabels)
	}
}

// TestWorker_LateChunkActive_NonTransientTokenErrorFailsTheDocument pins SPEC
// 8.1.9 "Failure classification" as revised: a NON-transient token-embedding
// failure of one document is a TERMINAL failure of every chunk of that document,
// never a fall back to chunk-then-embed. No chunk of it may be embedded by any
// path, no vector may reach the index, and the recorded reason names the cause
// and the remediation so the chunks can be requeued once it is fixed.
func TestWorker_LateChunkActive_NonTransientTokenErrorFailsTheDocument(t *testing.T) {
	emb := &lcWordEmbedder{tokenErr: &model.ProviderError{Code: "TEI_FAILED", Message: "Validation: too long", Retryable: false, StatusCode: http.StatusUnprocessableEntity}}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "gamma delta", 11, 22),
		}},
		texts: map[int64]string{7: "alpha beta gamma delta"},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	n, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0 (the whole document failed)", n)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("a failed document must NOT fall back to chunk-then-embed: embedInputs=%q", emb.embedInputs)
	}
	if len(ix.vectors) != 0 {
		t.Fatalf("no vector may reach the index for a failed document: %v", ix.vectors)
	}
	if len(src.embedded) != 0 {
		t.Fatalf("no chunk may be marked embedded: %v", src.embedded)
	}
	if len(src.failedLabels) != 2 || src.failedLabels[0] != 1 || src.failedLabels[1] != 2 {
		t.Fatalf("every chunk of the document must be marked failed, got %v", src.failedLabels)
	}
	if !strings.Contains(src.failedReason, "token embedding") || !strings.Contains(src.failedReason, "dir2mcp reindex") {
		t.Fatalf("reason must name the cause and the remediation: %q", src.failedReason)
	}
	if strings.Contains(src.failedReason, "falling back") {
		t.Fatalf("reason must not describe a fallback: %q", src.failedReason)
	}
	if src.failedCategory == "" {
		t.Fatal("the failure must carry a category (store.ClassifyError)")
	}
	if !strings.Contains(buf.String(), "marking its 2 chunk(s) failed") {
		t.Fatalf("the terminal document failure must be logged: %q", buf.String())
	}
}

// TestWorker_LateChunkActive_OneDocumentFailureLeavesSiblingsPooled pins that a
// document's terminal failure is scoped to that document: another
// representation in the same batch still pools and indexes normally.
func TestWorker_LateChunkActive_OneDocumentFailureLeavesSiblingsPooled(t *testing.T) {
	emb := &lcWordEmbedder{failRepText: "bad document here"}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 8, "bad document", 0, 12),
		}},
		texts: map[int64]string{7: "alpha beta gamma", 8: "bad document here"},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	n, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1 (the healthy document)", n)
	}
	if got, want := ix.vectors[1], unit(1.5, 1); !vecClose(got, want) {
		t.Fatalf("healthy chunk = %v, want pooled %v", got, want)
	}
	if _, indexed := ix.vectors[2]; indexed {
		t.Fatal("the failed document's chunk must not be indexed")
	}
	if len(src.failedLabels) != 1 || src.failedLabels[0] != 2 {
		t.Fatalf("only the failed document's chunk may be marked failed: %v", src.failedLabels)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("no chunk-then-embed fallback anywhere: %q", emb.embedInputs)
	}
}

// TestWorker_LateChunkActive_RepresentationsOfOneFilePoolIndependently pins SPEC
// 8.1.9 "Pooling": the unit is the text REPRESENTATION. Two representations of
// one file (a raw_text and an extracted_markdown, same rel_path) each embed their
// OWN persisted text once and pool only their own chunks, so a chunk is never
// pooled against another representation's text.
func TestWorker_LateChunkActive_RepresentationsOfOneFilePoolIndependently(t *testing.T) {
	// rep 7: "alpha beta" -> tokens [1,1] [2,1]; rep 8: "x y z" -> [1,1] [2,1] [3,1].
	const rawText = "alpha beta"
	const mdText = "x y z"
	emb := &lcWordEmbedder{}
	taskA := lcTask(1, 7, "alpha beta", 0, 10)
	taskA.Metadata.RepType = "raw_text"
	taskB := lcTask(2, 8, "y z", 2, 5)
	taskB.Metadata.RepType = "extracted_markdown"
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{taskA, taskB}},
		texts:           map[int64]string{7: rawText, 8: mdText},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	if _, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if emb.tokenCalls != 2 {
		t.Fatalf("each representation must be token-embedded once: tokenCalls=%d docs=%q", emb.tokenCalls, emb.tokenDocs)
	}
	seen := map[string]bool{}
	for _, d := range emb.tokenDocs {
		seen[d] = true
	}
	if !seen[rawText] || !seen[mdText] {
		t.Fatalf("each representation must embed its OWN text, got %q", emb.tokenDocs)
	}
	// Chunk 1 pools rep 7's tokens 1..2 -> mean [1.5,1]; chunk 2 pools rep 8's
	// tokens 2..3 -> mean [2.5,1]. Pooling chunk 2 against rep 7's text would
	// yield [2,1] (only its second token overlaps), so the values separate the
	// per-representation unit from a shared-document one.
	if got, want := ix.vectors[1], unit(1.5, 1); !vecClose(got, want) {
		t.Fatalf("raw_text chunk = %v, want %v from its own text", got, want)
	}
	if got, want := ix.vectors[2], unit(2.5, 1); !vecClose(got, want) {
		t.Fatalf("extracted_markdown chunk = %v, want %v from ITS own text (never the other representation's)", got, want)
	}
	if emb.embedCalls != 0 {
		t.Fatalf("both chunks must pool, not embed: %q", emb.embedInputs)
	}
}

// TestWorker_LateChunkActive_SpanWithoutTokensFallsBackPerChunk pins the
// per-chunk fallback (SPEC 8.1.9 "Pooling"): a chunk whose span no token
// overlaps is embedded chunk-then-embed on its own, while its siblings pool.
func TestWorker_LateChunkActive_SpanWithoutTokensFallsBackPerChunk(t *testing.T) {
	const doc = "alpha beta  gamma" // two spaces: runes [10,12) hold no token
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "  ", 10, 12),
			lcTask(3, 7, "gamma", 12, 17),
		}},
		texts: map[int64]string{7: doc},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	n, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed = %d, want 3", n)
	}
	if emb.embedCalls != 1 || len(emb.embedInputs) != 1 || emb.embedInputs[0] != "  " {
		t.Fatalf("only the token-less chunk may go through Embed: calls=%d inputs=%q", emb.embedCalls, emb.embedInputs)
	}
	if got, want := ix.vectors[1], unit(1.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 1 = %v, want pooled %v", got, want)
	}
	if !vecClose(ix.vectors[2], []float32{0, 1}) {
		t.Fatalf("chunk 2 = %v, want the Embed fallback vector", ix.vectors[2])
	}
	if got, want := ix.vectors[3], unit(3, 1); !vecClose(got, want) {
		t.Fatalf("chunk 3 = %v, want pooled %v", got, want)
	}
	if !strings.Contains(buf.String(), "overlap no token") {
		t.Fatalf("the per-chunk fallback must be logged: %q", buf.String())
	}
}

// TestWorker_LateChunkActive_TwoDocumentsPoolSeparately pins the grouping: a
// batch holding chunks of two representations embeds each document once and
// pools each chunk from its own document, never its neighbour's.
func TestWorker_LateChunkActive_TwoDocumentsPoolSeparately(t *testing.T) {
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 8, "x", 0, 1),
			lcTask(3, 7, "gamma delta", 11, 22),
		}},
		texts: map[int64]string{7: "alpha beta gamma delta", 8: "x"},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	if _, err := lcWorker(src, ix, emb, &buf).RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if emb.tokenCalls != 2 || emb.embedCalls != 0 {
		t.Fatalf("tokenCalls=%d embedCalls=%d, want 2/0 (one token embed per document)", emb.tokenCalls, emb.embedCalls)
	}
	if got, want := ix.vectors[3], unit(3.5, 1); !vecClose(got, want) {
		t.Fatalf("chunk 3 = %v, want %v from ITS document", got, want)
	}
	if got, want := ix.vectors[2], unit(1, 1); !vecClose(got, want) {
		t.Fatalf("chunk 2 = %v, want %v from the second document", got, want)
	}
}

// TestWorker_LateChunkActive_SourceWithoutTextIsFatal pins that a chunk source
// which cannot supply representation text stops the worker (ErrFatal) instead
// of running a "pooled" corpus through a path that cannot pool.
func TestWorker_LateChunkActive_SourceWithoutTextIsFatal(t *testing.T) {
	emb := &lcWordEmbedder{}
	src := &fakeChunkSource{tasks: []model.ChunkTask{lcTask(1, 7, "alpha beta", 0, 10)}}
	var buf bytes.Buffer
	_, err := lcWorker(src, newCapturingIndex(), emb, &buf).RunOnce(context.Background(), "text")
	if !errors.Is(err, index.ErrFatal) {
		t.Fatalf("want ErrFatal for a source without representation text, got %v", err)
	}
	if emb.tokenCalls != 0 || emb.embedCalls != 0 {
		t.Fatal("no embed call may be made")
	}
}

// TestWorker_LateChunkOff_TokenEmbedderStillChunkThenEmbeds pins the default:
// with the flag off a token-capable embedder is used through Embed only, and
// rune spans / representation text are never consulted.
func TestWorker_LateChunkOff_TokenEmbedderStillChunkThenEmbeds(t *testing.T) {
	emb := &lcWordEmbedder{}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{lcTask(1, 7, "alpha beta", -1, -1)}},
		texts:           map[int64]string{},
	}
	var buf bytes.Buffer
	w := lcWorker(src, newCapturingIndex(), emb, &buf)
	w.LateChunking = false
	n, err := w.RunOnce(context.Background(), "text")
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v; want 1, nil", n, err)
	}
	if emb.tokenCalls != 0 || emb.embedCalls != 1 {
		t.Fatalf("tokenCalls=%d embedCalls=%d, want 0/1 with the mode off", emb.tokenCalls, emb.embedCalls)
	}
	if len(src.failedLabels) != 0 {
		t.Fatalf("no chunk may fail for a missing span with the mode off: %v", src.failedLabels)
	}
	if buf.Len() != 0 && strings.Contains(buf.String(), "late chunking") {
		t.Fatalf("no late-chunking log with the mode off: %q", buf.String())
	}
}

// TestWorker_LateChunkOn_PlainEmbedderFallsBackHonestly pins the capability
// fallback (SPEC 8.1.4/8.1.9): the mode is on but the embedder lacks token
// embeddings, so the worker embeds chunk-then-embed, consults no spans, and the
// one-time log names the reason rather than claiming the mode is active.
func TestWorker_LateChunkOn_PlainEmbedderFallsBackHonestly(t *testing.T) {
	src := &fakeChunkSource{tasks: []model.ChunkTask{lcTask(1, 7, "alpha beta", -1, -1)}}
	var buf bytes.Buffer
	n, err := lcWorker(src, newCapturingIndex(), lcPlainEmbedder{}, &buf).RunOnce(context.Background(), "text")
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v; want 1, nil", n, err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "falling back to chunk-then-embed") || !strings.Contains(logged, string(latechunk.FallbackNoTokenEmbedder)) {
		t.Fatalf("log must name the fallback reason: %q", logged)
	}
	if strings.Contains(logged, "enabled and active") {
		t.Fatalf("log must not claim the mode is active: %q", logged)
	}
}

// TestWorker_LateChunkTask_HasRuneSpan pins the span validity rule the worker
// relies on: a known span needs a non-negative start and an end past it.
func TestWorker_LateChunkTask_HasRuneSpan(t *testing.T) {
	for _, tc := range []struct {
		start, end int
		want       bool
	}{
		{0, 1, true}, {5, 9, true}, {-1, -1, false}, {0, 0, false}, {3, 3, false}, {4, 2, false}, {-1, 5, false},
	} {
		if got := lcTask(1, 1, "x", tc.start, tc.end).HasRuneSpan(); got != tc.want {
			t.Errorf("HasRuneSpan(%d,%d) = %v, want %v", tc.start, tc.end, got, tc.want)
		}
	}
	if utf8.RuneCountInString("héllo") != 5 {
		t.Fatal("sanity: rune counting")
	}
}

// lcProbedEmbedder is a token embedder whose model.TokenEmbeddingProbe answers
// as configured: refuse with a reason, fail transiently, or accept. It counts
// probe calls so a test can pin "once per run".
type lcProbedEmbedder struct {
	lcWordEmbedder
	refuseReason string
	probeErr     error
	probeCalls   int
}

func (e *lcProbedEmbedder) TokenEmbeddingsAvailable(_ context.Context) (bool, string, error) {
	e.probeCalls++
	if e.probeErr != nil {
		return false, "", e.probeErr
	}
	if e.refuseReason != "" {
		return false, e.refuseReason, nil
	}
	return true, "", nil
}

// TestWorker_LateChunkProbe_RefusalFallsBackCorpusWide pins SPEC 8.1.9 for a
// served model that cannot pool (a tei server with cls pooling): the probe's
// refusal is the CORPUS-WIDE fall back, decided before any document is embedded.
// Nothing is token-embedded, nothing is marked failed, every chunk embeds
// chunk-then-embed, the once-per-run log names the provider's reason, and the
// probe is asked once, not once per batch (#951 review: the docs promised
// this, the code marked chunks failed instead).
func TestWorker_LateChunkProbe_RefusalFallsBackCorpusWide(t *testing.T) {
	emb := &lcProbedEmbedder{refuseReason: `late chunking requires a mean-pooling model; "bge-small" serves pooling "cls"`}
	src := &lcTextSource{
		fakeChunkSource: fakeChunkSource{tasks: []model.ChunkTask{
			lcTask(1, 7, "alpha beta", 0, 10),
			lcTask(2, 7, "gamma delta", 11, 22),
		}},
		texts: map[int64]string{7: "alpha beta gamma delta"},
	}
	ix := newCapturingIndex()
	var buf bytes.Buffer
	w := lcWorker(src, ix, emb, &buf)
	dec := w.LateChunkDecision()
	if dec.Active || dec.Fallback != latechunk.FallbackProviderRefused || !strings.Contains(dec.Detail, "cls") {
		t.Fatalf("decision = %+v, want inactive, %s, detail naming cls", dec, latechunk.FallbackProviderRefused)
	}
	n, err := w.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2 (chunk-then-embed for the whole corpus)", n)
	}
	if emb.tokenCalls != 0 {
		t.Fatal("a refused provider must never be asked for token embeddings")
	}
	if emb.embedCalls == 0 {
		t.Fatal("the chunks must embed chunk-then-embed")
	}
	if len(src.failedLabels) != 0 {
		t.Fatalf("a corpus-wide fall back marks nothing failed, got %v", src.failedLabels)
	}
	if emb.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want exactly 1 (cached for the run)", emb.probeCalls)
	}
	logged := buf.String()
	if !strings.Contains(logged, "falling back to chunk-then-embed") || !strings.Contains(logged, "cls") {
		t.Fatalf("the once-per-run log must name the fall back and the served pooling: %q", logged)
	}
	if strings.Contains(logged, "enabled and active") {
		t.Fatalf("the log must not claim the pooled path is active: %q", logged)
	}
}

// TestWorker_LateChunkProbe_UnknownKeepsThePooledPath pins the other half: a
// probe that cannot answer (the server is unreachable) is NOT a refusal. The
// decision stays Active, the probe is retried on the next decision, and the
// batch is neither degraded to chunk-then-embed nor marked failed: a transient
// outage must never flip a pooled corpus to unpooled vectors.
func TestWorker_LateChunkProbe_UnknownKeepsThePooledPath(t *testing.T) {
	emb := &lcProbedEmbedder{probeErr: &model.ProviderError{Code: "TEI_UNAVAILABLE", Message: "connection refused", Retryable: true, StatusCode: http.StatusServiceUnavailable}}
	var buf bytes.Buffer
	w := lcWorker(&lcTextSource{}, newCapturingIndex(), emb, &buf)
	if dec := w.LateChunkDecision(); !dec.Active {
		t.Fatalf("an unknown probe answer must keep the pooled path, got %+v", dec)
	}
	if dec := w.LateChunkDecision(); !dec.Active {
		t.Fatalf("still active on the second decision, got %+v", dec)
	}
	if emb.probeCalls != 2 {
		t.Fatalf("probe calls = %d, want 2: an unknown answer is not cached", emb.probeCalls)
	}
	// Once the server answers, the verdict is cached and the probe stops.
	emb.probeErr = nil
	for i := 0; i < 3; i++ {
		if dec := w.LateChunkDecision(); !dec.Active {
			t.Fatalf("decision %d: %+v", i, dec)
		}
	}
	if emb.probeCalls != 3 {
		t.Fatalf("probe calls = %d, want 3: a confirmed answer is cached", emb.probeCalls)
	}
}
