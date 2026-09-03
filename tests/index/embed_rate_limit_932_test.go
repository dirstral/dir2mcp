package tests

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #932, measured in production: a Gemini free-tier quota ran out mid-run
// and the worker marked 406 healthy chunks embedding_status=error. A chunk in
// that state disappears from BOTH retrieval paths with no query-time error,
// survives a provider switch, and only a manual reindex brings it back. On the
// pilot corpus it deleted an entire inning of a baseball game from search.
//
// The cause was that the worker decided transient-vs-permanent by searching the
// flattened message for "rate limit", while the provider had already answered
// the question structurally (ProviderError.Retryable / StatusCode 429). These
// tests pin the structured verdict first, the corrected keyword fallback
// second, and — most importantly — that the poison-chunk bisector (#399) is
// still reached by the genuine bad input it was built for.

// rateLimitEmbedder fails every batch with one provider's rate-limit shape and
// counts calls, because the count is the evidence: a transient failure costs
// exactly ONE embed call, while a batch routed into bisection costs several as
// it halves its way down to the individual chunks.
type rateLimitEmbedder struct {
	err   error
	calls int
}

func (e *rateLimitEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	e.calls++
	return nil, e.err
}

// providerRateLimit builds the error a provider adapter actually returns on a
// 429 (see internal/gemini, internal/mistral, internal/openai: every one sets
// Retryable true and StatusCode 429 for this case).
func providerRateLimit(code, msg string) error {
	return &model.ProviderError{Code: code, Message: msg, Retryable: true, StatusCode: http.StatusTooManyRequests}
}

func rateLimitTasks() []model.ChunkTask {
	return []model.ChunkTask{
		textTask(101, "healthy one"),
		textTask(102, "healthy two"),
		textTask(103, "healthy three"),
		textTask(104, "healthy four"),
	}
}

// The core contract: a rate-limited batch leaves EVERY chunk pending, marks
// nothing failed, never bisects, and surfaces the error so the run loop backs
// off. Run across the shapes of every provider that can embed here, because the
// bug was provider-specific spelling and a fix that only understands one
// provider is the same bug waiting for the next one.
func TestRateLimitedBatchLeavesEveryChunkPending_932(t *testing.T) {
	cases := map[string]error{
		// The exact production shape from the issue: the code spells it with an
		// underscore and the message reverses "quota exceeded", so neither
		// matched the old keyword.
		"gemini quota (structured)": providerRateLimit("GEMINI_RATE_LIMIT",
			`{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details."}}`),
		"mistral 429 (structured)": providerRateLimit("MISTRAL_RATE_LIMIT", "Requests rate limit exceeded"),
		"openai insufficient_quota (structured)": providerRateLimit("OPENAI_RATE_LIMIT",
			`{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`),
		// A 429 whose adapter forgot the Retryable flag: the status alone must
		// still route it correctly.
		"429 without the retryable flag": &model.ProviderError{
			Code: "SOME_PROVIDER_LIMIT", Message: "slow down", StatusCode: http.StatusTooManyRequests},
		// ONLY the structured verdict can save this one, and it is a real
		// shape: internal/gemini/client.go returns exactly
		// `GEMINI_FAILED / "request failed" / Retryable: true` when the HTTP
		// round trip itself fails. There is no status to read and the prose
		// matches no keyword in any table, so a worker that consults only
		// strings calls a retryable transport failure PERMANENT and bisects
		// the batch into dead chunks. (A mutation that removed the structured
		// branch passed the rest of this suite; this case is what fails it.)
		"provider says retryable, prose says nothing": &model.ProviderError{
			Code: "GEMINI_FAILED", Message: "request failed", Retryable: true},
		// No ProviderError at all (a plain error from a third-party or
		// OpenAI-compatible endpoint): the keyword fallback must catch it.
		"plain error, gemini wording": errors.New(
			`GEMINI_RATE_LIMIT: {"error":{"code":429,"message":"You exceeded your current quota"}}`),
		"plain error, code form only": errors.New("embed failed: gemini_rate_limit"),
	}
	for name, provErr := range cases {
		t.Run(name, func(t *testing.T) {
			src := &fakeChunkSource{}
			emb := &rateLimitEmbedder{err: provErr}
			worker := &index.EmbeddingWorker{Source: src, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 8}

			n, err := worker.EmbedAndIndex(context.Background(), "text", rateLimitTasks())
			if err == nil {
				t.Fatal("a rate limit must surface so the run loop backs off and retries")
			}
			if n != 0 {
				t.Fatalf("indexed = %d, want 0", n)
			}
			// The whole point of the issue: nothing may be marked failed.
			if len(src.failedLabels) != 0 {
				t.Fatalf("marked failed = %v, want none: a rate limit is a property of the ACCOUNT, "+
					"so failing these chunks deletes healthy content from the corpus", src.failedLabels)
			}
			if len(src.embedded) != 0 {
				t.Fatalf("embedded = %v, want none", src.embedded)
			}
			// One call means the batch was returned, not bisected. This is the
			// assertion that would have caught #932: the old code answered this
			// with 7 calls (4 chunks halved to individuals) and 4 dead chunks.
			if emb.calls != 1 {
				t.Fatalf("embed calls = %d, want exactly 1: a rate limit must never enter the bisector, "+
					"which would halve the batch down and mark each chunk failed", emb.calls)
			}
		})
	}
}

// The regression guard in the other direction. The fix must not turn the
// bisector off: a genuine provider input rejection (400, Retryable false) is
// still non-transient, still isolated, and still marks only the offending
// chunk (#399). A fix that made everything "transient" would trade a corpus
// that loses chunks for one that retries a permanently bad chunk forever.
func TestGenuinePoisonStillBisects_932(t *testing.T) {
	src := &fakeChunkSource{}
	emb := &poisonEmbedder{}
	worker := &index.EmbeddingWorker{Source: src, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(201, "healthy one"),
		textTask(202, poisonText),
		textTask(203, "healthy three"),
	}
	n, err := worker.EmbedAndIndex(context.Background(), "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex = %v, want nil once the poison chunk is isolated", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2 healthy siblings", n)
	}
	if len(src.failedLabels) != 1 || src.failedLabels[0] != 202 {
		t.Fatalf("failed = %v, want exactly [202]", src.failedLabels)
	}
	if emb.calls < 2 {
		t.Fatalf("embed calls = %d, want >=2 (the bisector must still run)", emb.calls)
	}
}

// A structured 400 carries Retryable false, so the structured branch must not
// swallow it: the same poison-isolation path applies whether the provider
// returned a bare string or a ProviderError.
func TestStructuredNonRetryableStillFails_932(t *testing.T) {
	src := &fakeChunkSource{}
	badInput := &model.ProviderError{
		Code: "GEMINI_FAILED", Message: "input exceeds maximum context length",
		Retryable: false, StatusCode: http.StatusBadRequest,
	}
	emb := &rateLimitEmbedder{err: badInput}
	worker := &index.EmbeddingWorker{Source: src, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 8}

	tasks := rateLimitTasks()
	n, err := worker.EmbedAndIndex(context.Background(), "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex = %v, want nil once every chunk is terminally marked", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(src.failedLabels) != len(tasks) {
		t.Fatalf("failed = %v, want all %d marked: a non-retryable provider rejection must stay "+
			"permanent, or a genuinely bad chunk retries forever", src.failedLabels, len(tasks))
	}
	if emb.calls < 2 {
		t.Fatalf("embed calls = %d, want >=2 (a non-retryable batch error must still bisect)", emb.calls)
	}
}

// Auth is the other account-wide failure, and it deliberately does NOT bisect
// (bisecting a 401 isolates nothing and hammers the provider with O(n) calls).
// It must also stay non-transient: an endless pending queue would hide a wrong
// API key instead of reporting it.
func TestAuthIsNeitherTransientNorBisected_932(t *testing.T) {
	src := &fakeChunkSource{}
	authErr := &model.ProviderError{
		Code: "GEMINI_AUTH", Message: "API key not valid",
		Retryable: false, StatusCode: http.StatusUnauthorized,
	}
	emb := &rateLimitEmbedder{err: authErr}
	worker := &index.EmbeddingWorker{Source: src, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 8}

	_, err := worker.EmbedAndIndex(context.Background(), "text", rateLimitTasks())
	if err == nil {
		t.Fatal("an auth failure must surface")
	}
	if len(src.failedLabels) != len(rateLimitTasks()) {
		t.Fatalf("failed = %v, want the whole batch marked (auth rejects every input identically)", src.failedLabels)
	}
	if emb.calls != 1 {
		t.Fatalf("embed calls = %d, want exactly 1 (auth must not bisect)", emb.calls)
	}
	if got := src.failedCategory; got != "auth" {
		t.Fatalf("category = %q, want %q", got, "auth")
	}
}
