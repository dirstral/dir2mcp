package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §9.10 (spec 0.59.0): the attributes filter on the tool surface. The
// recording retriever pins that the argument reaches the query verbatim; the
// validation cases pin that a malformed shape is INVALID_FIELD rather than a
// silently coerced filter; and the serialization case pins the §9.10 MUST that
// a hit's span surfaces the attributes it was filtered by.

type attrRecordingRetriever struct {
	got  model.SearchQuery
	hits []model.SearchHit
}

func (r *attrRecordingRetriever) Search(_ context.Context, q model.SearchQuery) ([]model.SearchHit, error) {
	r.got = q
	return r.hits, nil
}
func (r *attrRecordingRetriever) Ask(_ context.Context, question string, q model.SearchQuery) (model.AskResult, error) {
	r.got = q
	return model.AskResult{Question: question, Answer: "a", Citations: []model.Citation{}, Hits: r.hits, IndexingComplete: true}, nil
}
func (r *attrRecordingRetriever) OpenFile(_ context.Context, _ string, _ model.Span, _ int) (string, error) {
	return "", nil
}
func (r *attrRecordingRetriever) Stats(_ context.Context) (model.Stats, error) {
	return model.Stats{}, nil
}
func (r *attrRecordingRetriever) IndexingComplete(_ context.Context) (bool, error) {
	return true, nil
}

func attrCall(t *testing.T, ret *attrRecordingRetriever, tool, arguments string) (int, string) {
	t.Helper()
	cfg := config.Default()
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, ret).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+arguments+`}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
	return resp.StatusCode, string(body)
}

func TestAttributes928_ArgumentReachesTheQueryVerbatim(t *testing.T) {
	for _, tool := range []string{"dir2mcp_search", "dir2mcp_ask"} {
		ret := &attrRecordingRetriever{}
		args := `{"query":"q","attributes":{"inning":["8"],"half":["bottom","top"]}}`
		if tool == "dir2mcp_ask" {
			args = `{"question":"q","attributes":{"inning":["8"],"half":["bottom","top"]}}`
		}
		_, body := attrCall(t, ret, tool, args)
		if strings.Contains(body, "INVALID_FIELD") {
			t.Fatalf("%s rejected a valid attributes filter: %s", tool, body)
		}
		if got := ret.got.Attributes; got["inning"][0] != "8" || len(got["half"]) != 2 {
			t.Fatalf("%s: attributes did not reach the query: %v", tool, got)
		}
	}
}

// Values must pass through VERBATIM: canonicalization is the producer's
// (§9.10), so the server must not trim, lowercase or otherwise "help" — a
// zero-padded chapter id or case-sensitive code is a legitimate value.
func TestAttributes928_ValuesAreNotNormalizedInFlight(t *testing.T) {
	ret := &attrRecordingRetriever{}
	attrCall(t, ret, "dir2mcp_search", `{"query":"q","attributes":{"Chapter":[" 08 "]}}`)
	if v := ret.got.Attributes["Chapter"]; len(v) != 1 || v[0] != " 08 " {
		t.Fatalf("the server altered a filter value in flight: %q", v)
	}
}

func TestAttributes928_MalformedShapesAreInvalidField(t *testing.T) {
	cases := map[string]string{
		"array not object": `{"query":"q","attributes":["inning"]}`,
		"string value":     `{"query":"q","attributes":{"inning":"8"}}`,
		"number in array":  `{"query":"q","attributes":{"inning":[8]}}`,
		"object in array":  `{"query":"q","attributes":{"inning":[{"v":"8"}]}}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			ret := &attrRecordingRetriever{}
			_, body := attrCall(t, ret, "dir2mcp_search", args)
			if !strings.Contains(body, "INVALID_FIELD") {
				t.Fatalf("shape %q was not rejected: %s", name, body)
			}
		})
	}
}

// Absent, {}, and key:[] are all "no constraint" (§9.10): a serialization
// quirk must reach the retriever as a disabled filter, never as an error and
// never as match-nothing at the transport layer.
func TestAttributes928_EmptyShapesPassThroughDisabled(t *testing.T) {
	for name, args := range map[string]string{
		"empty object": `{"query":"q","attributes":{}}`,
		"empty array":  `{"query":"q","attributes":{"inning":[]}}`,
		// An explicit null reads as omitted, pinned deliberately: it is how
		// every optional filter on these tools reads null (entities, events,
		// languages), and client libraries routinely serialize an absent
		// optional as null.
		"explicit null": `{"query":"q","attributes":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			ret := &attrRecordingRetriever{}
			_, body := attrCall(t, ret, "dir2mcp_search", args)
			if strings.Contains(body, "INVALID_FIELD") {
				t.Fatalf("%s was rejected: %s", name, body)
			}
		})
	}
}

// The §9.10 MUST: a hit whose annotation carries attributes serves them on its
// span, so the caller can render the scope it filtered by.
func TestAttributes928_HitSpanSurfacesTheAttributes(t *testing.T) {
	ret := &attrRecordingRetriever{hits: []model.SearchHit{{
		ChunkID: 1, RelPath: "game.mp4", Snippet: "pitch",
		Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 2000,
			Event: "pitch", Attributes: map[string]string{"inning": "8"}},
	}}}
	_, body := attrCall(t, ret, "dir2mcp_search", `{"query":"q","attributes":{"inning":["8"]}}`)
	var env struct {
		Result struct {
			StructuredContent struct {
				Hits []struct {
					Span map[string]json.RawMessage `json:"span"`
				} `json:"hits"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Result.StructuredContent.Hits) != 1 {
		t.Fatalf("expected 1 hit, body: %s", body)
	}
	raw, ok := env.Result.StructuredContent.Hits[0].Span["attributes"]
	if !ok {
		t.Fatalf("hit span omits the attributes it was filtered by: %s", body)
	}
	var attrs map[string]string
	if err := json.Unmarshal(raw, &attrs); err != nil || attrs["inning"] != "8" {
		t.Fatalf("span attributes wrong: %s err=%v", raw, err)
	}
}
