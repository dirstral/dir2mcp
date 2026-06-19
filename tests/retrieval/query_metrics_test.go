package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/usage"
)

// usageReportingEmbedder reports a fixed embed-stage token usage into the
// context sink (mirroring a real provider) while returning deterministic
// vectors so retrieval results stay stable.
type usageReportingEmbedder struct {
	*fakeRetrievalEmbedder
	prompt int64
}

func (e *usageReportingEmbedder) Embed(ctx context.Context, modelName string, role model.EmbedRole, texts []string) ([][]float32, error) {
	usage.Report(ctx, usage.StageEmbed, usage.Usage{PromptTokens: e.prompt, Reported: true})
	return e.fakeRetrievalEmbedder.Embed(ctx, modelName, role, texts)
}

// usageReportingGenerator reports generate-stage token usage into the sink.
type usageReportingGenerator struct {
	out                   string
	promptTok, completTok int64
}

func (g *usageReportingGenerator) Generate(ctx context.Context, _ string) (string, error) {
	usage.Report(ctx, usage.StageGenerate, usage.Usage{
		PromptTokens:     g.promptTok,
		CompletionTokens: g.completTok,
		Reported:         true,
	})
	return g.out, nil
}

type capturedEvent struct {
	level string
	event string
	data  interface{}
}

type eventCapture struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (c *eventCapture) emit(level, event string, data interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{level, event, data})
}

func (c *eventCapture) queryMetrics() []map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]interface{}
	for _, e := range c.events {
		if e.event == "query_metrics" {
			if m, ok := e.data.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func metricsTestService(t *testing.T, gen model.Generator, cap *eventCapture) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for id, vec := range map[uint64][]float32{1: {1, 0}, 2: {0.9, 0.1}, 3: {0.8, 0.2}} {
		addVec(t, idx, id, vec)
	}
	emb := &usageReportingEmbedder{
		fakeRetrievalEmbedder: &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
			"mistral-embed": {1, 0},
		}},
		prompt: 42,
	}
	svc := retrieval.NewService(nil, idx, emb, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "beta"})
	svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "c.md", DocType: "md", Snippet: "gamma"})
	svc.SetGenerationModel("mistral-small-2506")
	svc.SetMetricsEmitter(cap.emit, usage.DefaultPriceTable())
	return svc
}

func TestQueryMetrics_SearchEmitsSingleEventWithFields(t *testing.T) {
	cap := &eventCapture{}
	svc := metricsTestService(t, nil, cap)

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 3, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}

	evs := cap.queryMetrics()
	if len(evs) != 1 {
		t.Fatalf("expected exactly 1 query_metrics event, got %d", len(evs))
	}
	ev := evs[0]
	if ev["op"] != "search" {
		t.Fatalf("op=%v, want search", ev["op"])
	}
	if _, ok := ev["latency_ms"].(int64); !ok {
		t.Fatalf("latency_ms missing/wrong type: %v", ev["latency_ms"])
	}
	if ev["total_tokens"].(int64) != 42 {
		t.Fatalf("total_tokens=%v, want 42 (embed)", ev["total_tokens"])
	}
	stages, ok := ev["stages"].(map[string]interface{})
	if !ok || stages["embed"] == nil {
		t.Fatalf("expected embed stage breakdown, got %v", ev["stages"])
	}
}

func TestQueryMetrics_AskEmitsSingleEventNotTwo(t *testing.T) {
	cap := &eventCapture{}
	gen := &usageReportingGenerator{out: "answer [a.md]", promptTok: 200, completTok: 80}
	svc := metricsTestService(t, gen, cap)

	res, err := svc.Ask(context.Background(), "what is alpha?", model.SearchQuery{Query: "alpha", K: 3, Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer == "" {
		t.Fatal("expected an answer")
	}

	evs := cap.queryMetrics()
	if len(evs) != 1 {
		t.Fatalf("ask must emit exactly 1 query_metrics event (no nested search event), got %d", len(evs))
	}
	ev := evs[0]
	if ev["op"] != "ask" {
		t.Fatalf("op=%v, want ask", ev["op"])
	}
	// embed(42) + generate(200+80) = 322
	if ev["total_tokens"].(int64) != 322 {
		t.Fatalf("total_tokens=%v, want 322", ev["total_tokens"])
	}
	cost, ok := ev["cost_usd"].(float64)
	if !ok {
		t.Fatalf("cost_usd should be present (mistral-embed + mistral-small-2506 priced): %v", ev["cost_usd"])
	}
	if cost <= 0 {
		t.Fatalf("cost_usd should be positive, got %v", cost)
	}
}

func TestQueryMetrics_ResultsUnchangedWithMetrics(t *testing.T) {
	// Same service/query with and without the metrics emitter must produce
	// identical hits: observability is additive and must not alter results.
	build := func(withMetrics bool) []model.SearchHit {
		idx := index.NewHNSWIndex("")
		for id, vec := range map[uint64][]float32{1: {1, 0}, 2: {0.9, 0.1}, 3: {0.8, 0.2}} {
			addVec(t, idx, id, vec)
		}
		svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
			"mistral-embed": {1, 0},
		}}, nil)
		svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
		svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "beta"})
		svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "c.md", DocType: "md", Snippet: "gamma"})
		if withMetrics {
			svc.SetMetricsEmitter((&eventCapture{}).emit, usage.DefaultPriceTable())
		}
		hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 3, Index: "text"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		return hits
	}

	with := build(true)
	without := build(false)
	if len(with) != len(without) {
		t.Fatalf("hit count differs: with=%d without=%d", len(with), len(without))
	}
	for i := range with {
		if with[i].ChunkID != without[i].ChunkID || with[i].Score != without[i].Score {
			t.Fatalf("hit %d differs: with=%+v without=%+v", i, with[i], without[i])
		}
	}
}

func TestQueryMetrics_DisabledEmitsNothing(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &usageReportingEmbedder{
		fakeRetrievalEmbedder: &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}},
		prompt:                42,
	}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
	// No SetMetricsEmitter call: metrics disabled.
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 1, Index: "text"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Nothing to assert beyond no panic / no emitter invoked; reaching here is success.
}
