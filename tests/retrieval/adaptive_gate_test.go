package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// newAdaptiveService builds a retrieval service whose index records the k it is
// asked for (fakeRetrievalIndex.lastK). Overfetch is pinned to 1 so lastK is
// exactly the effective k the gate chose (no multiplier to back out). The fake
// index returns no hits, which is all these tests need: they assert on the k
// requested and on whether the index was queried at all, not on results.
func newAdaptiveService(t *testing.T) (*retrieval.Service, *fakeRetrievalIndex) {
	t.Helper()
	idx := &fakeRetrievalIndex{lastK: -1}
	emb := &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}
	svc := retrieval.NewService(nil, idx, emb, nil)
	svc.SetOverfetchMultiplier(1)
	return svc, idx
}

// askEffectiveK runs Ask and returns the k the index was asked for. A returned
// value of -1 means the index was never queried (retrieval skipped).
func askEffectiveK(t *testing.T, svc *retrieval.Service, idx *fakeRetrievalIndex, question string) int {
	t.Helper()
	idx.lastK = -1
	if _, err := svc.Ask(context.Background(), question, model.SearchQuery{Index: "text"}); err != nil {
		t.Fatalf("Ask(%q): %v", question, err)
	}
	return idx.lastK
}

// TestAdaptive_Disabled_UsesFixedK pins the default-off contract: with the gate
// disabled, Ask uses the fixed default k (15) regardless of query shape — a
// trivial greeting and a hard multi-clause question request the same k, and the
// index is always queried.
func TestAdaptive_Disabled_UsesFixedK(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	// gate left unset (disabled by default).

	if got := askEffectiveK(t, svc, idx, "hi"); got != 15 {
		t.Fatalf("disabled gate: trivial query should use fixed k=15, got %d", got)
	}
	hard := "compare and contrast the performance and reliability tradeoffs between approach one and approach two in detail"
	if got := askEffectiveK(t, svc, idx, hard); got != 15 {
		t.Fatalf("disabled gate: hard query should use fixed k=15, got %d", got)
	}
}

// TestAdaptive_Skip_TrivialQuery pins the skip verdict: an enabled gate must NOT
// query the index for a pure conversational greeting (no information need).
func TestAdaptive_Skip_TrivialQuery(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	svc.SetAdaptiveRetrieval(true, 4, 30)

	if got := askEffectiveK(t, svc, idx, "thanks"); got != -1 {
		t.Fatalf("enabled gate: trivial query should skip retrieval (lastK -1), got %d", got)
	}
	// A real short question must NOT be skipped even when enabled.
	if got := askEffectiveK(t, svc, idx, "what is x402?"); got == -1 {
		t.Fatalf("enabled gate: a real question must not be skipped")
	}
}

// TestAdaptive_Narrow_EasyQuery pins the narrow verdict: an enabled gate biases
// k toward k_min for a short, single-clause question.
func TestAdaptive_Narrow_EasyQuery(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	svc.SetAdaptiveRetrieval(true, 4, 30)

	got := askEffectiveK(t, svc, idx, "what is x402?")
	if got != 4 {
		t.Fatalf("enabled gate: easy question should narrow k to k_min=4, got %d", got)
	}
}

// TestAdaptive_Widen_HardQuery pins the widen verdict: an enabled gate biases k
// toward k_max for a long, multi-clause/comparative question.
func TestAdaptive_Widen_HardQuery(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	svc.SetAdaptiveRetrieval(true, 4, 30)

	hard := "compare and contrast the performance and reliability tradeoffs between approach one and approach two in detail"
	got := askEffectiveK(t, svc, idx, hard)
	if got != 30 {
		t.Fatalf("enabled gate: hard question should widen k to k_max=30, got %d", got)
	}
}

// TestAdaptive_Default_ClampsBaseK pins the default verdict: a medium query that
// is neither easy nor hard keeps the base k (here the Ask default of 15) clamped
// into the configured window. With a window of [4,10] the base 15 clamps to 10.
func TestAdaptive_Default_ClampsBaseK(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	svc.SetAdaptiveRetrieval(true, 4, 10)

	// A statement (no interrogative, no connectors, mid length) → default class.
	got := askEffectiveK(t, svc, idx, "the indexing pipeline handles multimodal documents end to end")
	if got != 10 {
		t.Fatalf("enabled gate: default class should clamp base k into window (10), got %d", got)
	}
}

// TestAdaptive_BoundsSanitized pins that SetAdaptiveRetrieval repairs an
// unusable window: a 0 (use default) max and an inverted min/max never produce
// an out-of-range or panicking k. With min=8 and max=0 (→ default 30), a hard
// query widens to 30, not to 8.
func TestAdaptive_BoundsSanitized(t *testing.T) {
	svc, idx := newAdaptiveService(t)
	svc.SetAdaptiveRetrieval(true, 8, 0)

	hard := "compare and contrast the performance and reliability tradeoffs between approach one and approach two in detail"
	if got := askEffectiveK(t, svc, idx, hard); got != 30 {
		t.Fatalf("k_max=0 should fall back to built-in default 30, got %d", got)
	}
	if got := askEffectiveK(t, svc, idx, "what is x402?"); got != 8 {
		t.Fatalf("easy query should narrow to k_min=8, got %d", got)
	}
}
