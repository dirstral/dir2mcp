package tests

import (
	"context"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/usage"
)

func TestSink_ReportAndStage(t *testing.T) {
	s := usage.NewSink()
	s.Report(usage.StageGenerate, usage.Usage{PromptTokens: 100, CompletionTokens: 50, Reported: true})
	s.Report(usage.StageGenerate, usage.Usage{PromptTokens: 10, CompletionTokens: 5, Reported: true})

	u, ok := s.Stage(usage.StageGenerate)
	if !ok {
		t.Fatal("expected generate usage to be reported")
	}
	if u.PromptTokens != 110 || u.CompletionTokens != 55 {
		t.Fatalf("accumulation wrong: %+v", u)
	}
	if u.TotalTokens != 165 {
		t.Fatalf("derived total wrong: %d, want 165", u.TotalTokens)
	}
}

func TestSink_UnreportedStage(t *testing.T) {
	s := usage.NewSink()
	if _, ok := s.Stage(usage.StageEmbed); ok {
		t.Fatal("stage with no report must be unknown (ok=false)")
	}
}

func TestSink_NilIsNoOp(t *testing.T) {
	var s *usage.Sink
	s.Report(usage.StageEmbed, usage.Usage{PromptTokens: 1})
	s.AddLatency(usage.StageEmbed, time.Second)
	if _, ok := s.Stage(usage.StageEmbed); ok {
		t.Fatal("nil sink must report nothing")
	}
}

func TestSink_ContextRoundTrip(t *testing.T) {
	s := usage.NewSink()
	ctx := usage.WithSink(context.Background(), s)
	usage.Report(ctx, usage.StageEmbed, usage.Usage{PromptTokens: 7, Reported: true})
	u, ok := s.Stage(usage.StageEmbed)
	if !ok || u.PromptTokens != 7 {
		t.Fatalf("context-attached report failed: ok=%v %+v", ok, u)
	}
}

func TestSink_TimeStageRecordsLatency(t *testing.T) {
	s := usage.NewSink()
	err := s.TimeStage(usage.StageRerank, func() error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s.Latency(usage.StageRerank) <= 0 {
		t.Fatal("expected positive recorded latency")
	}
}

func TestQueryMetrics_EventKnownModelHasCost(t *testing.T) {
	pt := usage.NewPriceTable(map[string]usage.ModelPrice{
		"chat-x": {InputPer1K: 0.001, OutputPer1K: 0.002},
	})
	qm := usage.NewQueryMetrics("ask", pt)
	qm.RecordStage(usage.StageGenerate, "chat-x", 30*time.Millisecond,
		usage.Usage{PromptTokens: 1000, CompletionTokens: 500}, true)
	qm.SetTotalLatency(50 * time.Millisecond)

	ev := qm.Event()
	if ev["op"] != "ask" {
		t.Fatalf("op=%v, want ask", ev["op"])
	}
	if ev["latency_ms"].(int64) != 50 {
		t.Fatalf("latency_ms=%v, want 50", ev["latency_ms"])
	}
	if ev["total_tokens"].(int64) != 1500 {
		t.Fatalf("total_tokens=%v, want 1500", ev["total_tokens"])
	}
	cost, ok := ev["cost_usd"].(float64)
	if !ok {
		t.Fatalf("cost_usd missing for known model: %v", ev["cost_usd"])
	}
	if !approxEqual(cost, 0.002) {
		t.Fatalf("cost_usd=%v, want 0.002", cost)
	}
	stages, ok := ev["stages"].(map[string]interface{})
	if !ok || stages["generate"] == nil {
		t.Fatalf("expected generate stage breakdown, got %v", ev["stages"])
	}
}

func TestQueryMetrics_EventUnknownModelOmitsCost(t *testing.T) {
	qm := usage.NewQueryMetrics("ask", usage.DefaultPriceTable())
	qm.RecordStage(usage.StageGenerate, "unknown-model-zzz", 10*time.Millisecond,
		usage.Usage{PromptTokens: 1000, CompletionTokens: 1000}, true)
	ev := qm.Event()
	if _, present := ev["cost_usd"]; present {
		t.Fatalf("cost_usd must be omitted for unknown model, got %v", ev["cost_usd"])
	}
	// tokens and latency must still be present.
	if ev["total_tokens"].(int64) != 2000 {
		t.Fatalf("total_tokens=%v, want 2000", ev["total_tokens"])
	}
}

func TestQueryMetrics_LatencyOnlyStageNoTokens(t *testing.T) {
	qm := usage.NewQueryMetrics("search", usage.DefaultPriceTable())
	// rerank stage with latency but no reported tokens (e.g. Cohere rerank).
	qm.RecordStage(usage.StageRerank, "rerank-v3", 5*time.Millisecond, usage.Usage{}, false)
	qm.SetTotalLatency(8 * time.Millisecond)
	ev := qm.Event()
	if ev["total_tokens"].(int64) != 0 {
		t.Fatalf("expected 0 tokens, got %v", ev["total_tokens"])
	}
	if _, present := ev["cost_usd"]; present {
		t.Fatal("no priced tokens => cost_usd omitted")
	}
}
