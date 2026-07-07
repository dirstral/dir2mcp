package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

func skipSnapshot() corpusSnapshot {
	return corpusSnapshot{
		Timestamp: "2026-07-07T00:00:00Z",
		Indexing: corpusIndexing{
			Mode:    "incremental",
			Skipped: 3,
			SkipSummary: &model.SkipSummary{
				Categories: map[string]int64{
					model.SkipReasonArchive:    2,
					model.SkipReasonIgnoreRule: 1,
				},
			},
		},
		DocCounts: map[string]int64{"text": 1},
		TotalDocs: 1,
	}
}

func failuresFixture() []model.Document {
	return []model.Document{
		{RelPath: "broken.pdf", DocType: "pdf", MTimeUnix: 1, ErrorMessage: "docling crashed on page 3"},
		// A message carrying a credential must be redacted before display.
		{RelPath: "auth.log", DocType: "text", MTimeUnix: 2, ErrorMessage: "auth failed password=hunter2supersecret"},
	}
}

// TestRenderStatus_TextCoverageAndFailures verifies the text status output
// renders the honest-coverage block (per-reason skip breakdown) and the recent
// failures block, and that credentials in a failure message are redacted (#414).
func TestRenderStatus_TextCoverageAndFailures(t *testing.T) {
	var out bytes.Buffer
	app := NewAppWithIO(&out, &bytes.Buffer{})

	code := app.renderStatusOutput(globalOptions{}, "/state", skipSnapshot(), "computed", false, failuresFixture())
	if code != exitSuccess {
		t.Fatalf("renderStatusOutput code = %d, want %d", code, exitSuccess)
	}
	got := out.String()

	for _, want := range []string{"Coverage", "archive", "ignore_rule", "Recent failures", "broken.pdf", "auth.log"} {
		if !strings.Contains(got, want) {
			t.Errorf("status text missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2supersecret") {
		t.Errorf("status text leaked a credential from a failure message:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("status text did not redact the credential:\n%s", got)
	}
}

// TestRenderStatus_JSONIncludesSkipSummaryAndFailures verifies --json carries
// the skip_summary inside the snapshot and an additive recent_failures array
// with redacted messages.
func TestRenderStatus_JSONIncludesSkipSummaryAndFailures(t *testing.T) {
	var out bytes.Buffer
	app := NewAppWithIO(&out, &bytes.Buffer{})

	code := app.renderStatusOutput(globalOptions{jsonOutput: true}, "/state", skipSnapshot(), "computed", false, failuresFixture())
	if code != exitSuccess {
		t.Fatalf("renderStatusOutput code = %d, want %d", code, exitSuccess)
	}

	var payload struct {
		Snapshot struct {
			Indexing struct {
				SkipSummary *model.SkipSummary `json:"skip_summary"`
			} `json:"indexing"`
		} `json:"snapshot"`
		RecentFailures []map[string]interface{} `json:"recent_failures"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, out.String())
	}
	if payload.Snapshot.Indexing.SkipSummary == nil {
		t.Fatalf("snapshot.indexing.skip_summary missing:\n%s", out.String())
	}
	if payload.Snapshot.Indexing.SkipSummary.Categories[model.SkipReasonArchive] != 2 {
		t.Errorf("skip_summary archive count = %d, want 2", payload.Snapshot.Indexing.SkipSummary.Categories[model.SkipReasonArchive])
	}
	if len(payload.RecentFailures) != 2 {
		t.Fatalf("recent_failures len = %d, want 2:\n%s", len(payload.RecentFailures), out.String())
	}
	if msg, _ := payload.RecentFailures[1]["error_message"].(string); strings.Contains(msg, "hunter2supersecret") {
		t.Errorf("recent_failures json leaked a credential: %q", msg)
	}
}

// TestRenderStatus_HealthyCorpusOmitsBlocks verifies a corpus with nothing
// skipped and no failures renders neither the Coverage nor the Recent failures
// block, and emits no recent_failures field in JSON.
func TestRenderStatus_HealthyCorpusOmitsBlocks(t *testing.T) {
	healthy := corpusSnapshot{
		Timestamp: "2026-07-07T00:00:00Z",
		Indexing:  corpusIndexing{Mode: "incremental"},
		DocCounts: map[string]int64{"text": 1},
		TotalDocs: 1,
	}

	var textOut bytes.Buffer
	app := NewAppWithIO(&textOut, &bytes.Buffer{})
	app.renderStatusOutput(globalOptions{}, "/state", healthy, "computed", false, nil)
	if s := textOut.String(); strings.Contains(s, "Coverage") || strings.Contains(s, "Recent failures") {
		t.Errorf("healthy corpus rendered a skip/failure block:\n%s", s)
	}

	var jsonOut bytes.Buffer
	app2 := NewAppWithIO(&jsonOut, &bytes.Buffer{})
	app2.renderStatusOutput(globalOptions{jsonOutput: true}, "/state", healthy, "computed", false, nil)
	if strings.Contains(jsonOut.String(), "recent_failures") {
		t.Errorf("healthy corpus emitted recent_failures in json:\n%s", jsonOut.String())
	}
	if strings.Contains(jsonOut.String(), "skip_summary") {
		t.Errorf("healthy corpus emitted skip_summary in json:\n%s", jsonOut.String())
	}
}

// TestFormatSkipBreakdown merges the store's durable SkipSummary with the
// in-run (non-persisted) path-exclude counts into one stable, sorted line.
func TestFormatSkipBreakdown(t *testing.T) {
	if got := formatSkipBreakdown(nil, nil); got != "" {
		t.Errorf("empty breakdown = %q, want empty", got)
	}
	summary := &model.SkipSummary{Categories: map[string]int64{
		model.SkipReasonArchive: 2,
		model.SkipReasonSizeCap: 1,
	}}
	inRun := map[string]int64{
		model.SkipReasonPathExcluded: 5,
		model.SkipReasonArchive:      1, // summed with the durable count
	}
	got := formatSkipBreakdown(summary, inRun)
	want := "archive=3 path_excluded=5 size_cap=1"
	if got != want {
		t.Errorf("formatSkipBreakdown = %q, want %q", got, want)
	}
}
