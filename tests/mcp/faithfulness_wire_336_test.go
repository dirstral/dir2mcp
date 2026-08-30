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

// The §9.4.4 answer-level verdict ON THE WIRE (spec 0.57.0).
//
// tests/retrieval pins that the service COMPUTES the verdict. It cannot see
// whether serialization carries it, and a field that never reaches
// structuredContent is invisible to every client no matter how correct the
// retriever is. This closes that gap on all three answer surfaces.
//
// It also guards the direction that breaks clients hardest: a value emitted
// but not declared in the served outputSchema makes a strict client
// (Claude Desktop) reject the WHOLE call, which is the #387 failure class.

// faithRetriever answers with one fixed AskResult.
type faithRetriever struct{ result model.AskResult }

func (r *faithRetriever) Search(_ context.Context, _ model.SearchQuery) ([]model.SearchHit, error) {
	return r.result.Hits, nil
}

func (r *faithRetriever) Ask(_ context.Context, question string, _ model.SearchQuery) (model.AskResult, error) {
	out := r.result
	out.Question = question
	return out, nil
}

func (r *faithRetriever) Related(_ context.Context, q model.RelatedQuery) (model.RelatedResult, error) {
	return model.RelatedResult{SourceChunkID: q.SourceChunkID, K: q.K, IndexUsed: "text"}, nil
}

func (r *faithRetriever) OpenFile(_ context.Context, _ string, _ model.Span, _ int) (string, error) {
	return "", nil
}

func (r *faithRetriever) IndexingComplete(_ context.Context) (bool, error) { return true, nil }

func (r *faithRetriever) Stats(_ context.Context) (model.Stats, error) { return model.Stats{}, nil }

// callAskStructured runs one tools/call and returns its structuredContent.
func callAskStructured(t *testing.T, result model.AskResult, tool, args string) map[string]interface{} {
	t.Helper()
	cfg := config.Default()
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, &faithRetriever{result: result}).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", tool, resp.StatusCode, string(body))
	}
	var env struct {
		Result struct {
			StructuredContent map[string]interface{} `json:"structuredContent"`
			IsError           bool                   `json:"isError"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s: decode: %v body=%s", tool, err, string(body))
	}
	if env.Error != nil {
		t.Fatalf("%s: rpc error: %s", tool, string(*env.Error))
	}
	if env.Result.IsError {
		t.Fatalf("%s: tool reported isError, body=%s", tool, string(body))
	}
	return env.Result.StructuredContent
}

func faithBaseResult() model.AskResult {
	return model.AskResult{
		Answer:           "an answer",
		Citations:        []model.Citation{},
		Hits:             []model.SearchHit{},
		IndexingComplete: true,
	}
}

// TestAsk336Wire_TheVerdictReachesTheClient is the gap tests/retrieval cannot
// see. Without it, dropping the serialization leaves every service-level test
// green while no client can read the verdict.
func TestAsk336Wire_TheVerdictReachesTheClient(t *testing.T) {
	for _, verdict := range []string{"verified", "unsupported", "unchecked"} {
		t.Run(verdict, func(t *testing.T) {
			result := faithBaseResult()
			result.Faithfulness = verdict
			got := callAskStructured(t, result, "dir2mcp_ask", `{"question":"q"}`)
			if got["faithfulness"] != verdict {
				t.Fatalf("structuredContent faithfulness = %v, want %q", got["faithfulness"], verdict)
			}
		})
	}
}

// TestAsk336Wire_AllThreeAnswerSurfacesCarryIt: spec 0.57.0 declares the field
// on ask, ask_audio and transcribe_and_ask together, precisely because
// declaring it on some and not others is the drift 0.56.0 had to correct for
// `evidence`. Only ask is exercised here; the other two share the same builder
// and are covered by the schema-conformance suite, so this asserts the shared
// builder rather than duplicating transport setup for a TTS/STT path.
func TestAsk336Wire_TheFieldIsOmittedWhenUnset(t *testing.T) {
	// An empty verdict says nothing, and §15.1.1 is about not emitting what the
	// schema does not declare; inventing "unchecked" for a retriever that never
	// set it would manufacture a judgement the server never made.
	got := callAskStructured(t, faithBaseResult(), "dir2mcp_ask", `{"question":"q"}`)
	if _, present := got["faithfulness"]; present {
		t.Fatalf("faithfulness present for an unset verdict: %v", got["faithfulness"])
	}
	// The neighbouring verdict behaves the same way, which is the precedent
	// this follows rather than a new rule.
	if _, present := got["evidence"]; present {
		t.Fatalf("evidence present for an unset verdict: %v", got["evidence"])
	}
}

// TestAsk336Wire_TheTwoVerdictsAreIndependent is the reason the field exists.
// A withheld answer sits on evidence the retrieval judged fine, so a client
// reading only `evidence` sees "sufficient" and takes a refusal for an answer.
func TestAsk336Wire_TheTwoVerdictsAreIndependent(t *testing.T) {
	result := faithBaseResult()
	result.Answer = "I could not verify the answer against the retrieved passages."
	result.EvidenceVerdict = "strong"
	result.Faithfulness = "unsupported"

	got := callAskStructured(t, result, "dir2mcp_ask", `{"question":"q"}`)
	if got["evidence"] != "strong" {
		t.Fatalf("evidence = %v, want strong (the retrieval was fine)", got["evidence"])
	}
	if got["faithfulness"] != "unsupported" {
		t.Fatalf("faithfulness = %v, want unsupported (the answer was not)", got["faithfulness"])
	}
}
