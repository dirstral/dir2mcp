package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #1081, SPEC §15.6 (spec 0.76.0): dir2mcp_stats carries the optional
// `evidence` object when the retriever calibrates its threshold, with exactly
// the stats.json field names, and omits it otherwise.

type evidenceStatsRetriever struct {
	report model.EvidenceReport
}

func (r *evidenceStatsRetriever) Search(context.Context, model.SearchQuery) ([]model.SearchHit, error) {
	return nil, nil
}
func (r *evidenceStatsRetriever) Ask(_ context.Context, q string, _ model.SearchQuery) (model.AskResult, error) {
	return model.AskResult{Question: q}, nil
}
func (r *evidenceStatsRetriever) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", nil
}
func (r *evidenceStatsRetriever) Stats(context.Context) (model.Stats, error) {
	return model.Stats{}, model.ErrNotImplemented
}
func (r *evidenceStatsRetriever) IndexingComplete(context.Context) (bool, error) { return true, nil }

// evidenceStatsCalibrating adds the EvidenceReporter methods on top.
type evidenceStatsCalibrating struct {
	*evidenceStatsRetriever
	calibrates bool
}

func (r evidenceStatsCalibrating) CalibratesEvidence() bool { return r.calibrates }
func (r evidenceStatsCalibrating) EvidenceReport(context.Context) model.EvidenceReport {
	return r.report
}

func statsStructured(t *testing.T, retriever model.Retriever) map[string]interface{} {
	t.Helper()
	cfg := config.Default()
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("stats returned isError")
	}
	return envelope.Result.StructuredContent
}

func TestStats1081_EvidenceObjectWhenTheRetrieverCalibrates(t *testing.T) {
	report := model.EvidenceReport{
		CosineThreshold:       0.519,
		CosineThresholdSource: model.EvidenceThresholdSourceAuto,
		RerankThreshold:       0.02,
		Baseline: &model.EvidenceBaseline{
			Probes: 32, ProbeSet: "v1", P50: 0.478, P90: 0.519, Max: 0.548,
			Chunks: 94, EmbedModel: "nomic-embed-text:latest", ComputedAt: "2026-10-06T14:00:00Z",
		},
	}
	structured := statsStructured(t, evidenceStatsCalibrating{&evidenceStatsRetriever{report: report}, true})
	evidence, ok := structured["evidence"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats must carry evidence, got %#v", structured["evidence"])
	}
	if evidence["cosine_threshold"] != 0.519 || evidence["cosine_threshold_source"] != "auto" || evidence["rerank_threshold"] != 0.02 {
		t.Fatalf("evidence thresholds = %#v", evidence)
	}
	baseline, ok := evidence["null_baseline"].(map[string]interface{})
	if !ok {
		t.Fatalf("evidence.null_baseline missing: %#v", evidence)
	}
	for _, key := range []string{"probes", "probe_set", "p50", "p90", "max", "chunks", "embed_model", "computed_at"} {
		if _, present := baseline[key]; !present {
			t.Fatalf("null_baseline lacks %q: %#v", key, baseline)
		}
	}
	if baseline["probes"] != float64(32) || baseline["p90"] != 0.519 || baseline["embed_model"] != "nomic-embed-text:latest" {
		t.Fatalf("null_baseline values = %#v", baseline)
	}
	if len(baseline) != 8 {
		t.Fatalf("null_baseline carries %d fields, want the 8 of stats.json: %#v", len(baseline), baseline)
	}
}

func TestStats1081_NoBaselineYetOmitsTheObjectField(t *testing.T) {
	report := model.EvidenceReport{CosineThreshold: 0.05, CosineThresholdSource: model.EvidenceThresholdSourceFloor, RerankThreshold: 0.02}
	structured := statsStructured(t, evidenceStatsCalibrating{&evidenceStatsRetriever{report: report}, true})
	evidence, ok := structured["evidence"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats must carry evidence, got %#v", structured["evidence"])
	}
	if _, present := evidence["null_baseline"]; present {
		t.Fatalf("null_baseline must be omitted before it is computed: %#v", evidence)
	}
	if evidence["cosine_threshold_source"] != "floor" {
		t.Fatalf("source = %#v, want floor", evidence["cosine_threshold_source"])
	}
}

func TestStats1081_NonCalibratingRetrieverOmitsEvidence(t *testing.T) {
	structured := statsStructured(t, &evidenceStatsRetriever{})
	if _, present := structured["evidence"]; present {
		t.Fatalf("a retriever without EvidenceReport must not produce evidence: %#v", structured["evidence"])
	}
	// A reporter that is not configured to calibrate omits it too, and its
	// advertised schema does not declare it (the canonical contract of the
	// pinned spec closes the object).
	structured = statsStructured(t, evidenceStatsCalibrating{&evidenceStatsRetriever{}, false})
	if _, present := structured["evidence"]; present {
		t.Fatalf("a non-calibrating reporter must not produce evidence: %#v", structured["evidence"])
	}
}
