package conformance

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// toolCallResultEnvelope is the decoded shape of a tools/call response: the
// tool-level isError flag plus structuredContent, which carries the canonical
// error object (df-008) when the call failed.
type toolCallResultEnvelope struct {
	Result struct {
		IsError           bool `json:"isError"`
		StructuredContent struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"structuredContent"`
	} `json:"result"`
	Error interface{} `json:"error"`
}

// callToolRaw posts one tools/call with a raw JSON arguments object and returns
// the decoded tool-level envelope.
func callToolRaw(t *testing.T, mcpURL, sessionID, tool, argsJSON string) toolCallResultEnvelope {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` + tool +
		`","arguments":` + argsJSON + `}}`
	resp := sendRPC(t, mcpURL, sessionID, body, nil)
	payload := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d want=200 body=%s", tool, resp.StatusCode, payload)
	}
	var envelope toolCallResultEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("%s: decode: %v body=%s", tool, err, payload)
	}
	if envelope.Error != nil {
		t.Fatalf("%s: expected a tool-level result, got a JSON-RPC error: %s", tool, payload)
	}
	return envelope
}

// envelopeErrorCode returns the canonical error code of a failed tools/call, or
// "" when the call did not fail. Unlike toolErrorCode it never fails the test
// on a SUCCESSFUL call, so a caller can assert "this code did not occur".
func envelopeErrorCode(envelope toolCallResultEnvelope) string {
	if !envelope.Result.IsError || envelope.Result.StructuredContent.Error == nil {
		return ""
	}
	return envelope.Result.StructuredContent.Error.Code
}

// kBearingTools are every tool whose advertised input schema carries k with the
// "minimum": 1 / "maximum": 50 bound (SPEC §15.2/§15.3, canonical
// search.json/ask.json/related.json/ask_audio.json/transcribe_and_ask.json).
// argsWithK renders a minimal, otherwise-valid argument object for the tool
// with the given k, so the k check is the only thing under test.
var kBearingTools = []struct {
	name      string
	argsWithK func(k int) string
}{
	{
		name:      "dir2mcp_search",
		argsWithK: func(k int) string { return `{"query":"q","k":` + strconv.Itoa(k) + `}` },
	},
	{
		name:      "dir2mcp_ask",
		argsWithK: func(k int) string { return `{"question":"q","k":` + strconv.Itoa(k) + `}` },
	},
	{
		name:      "dir2mcp_related",
		argsWithK: func(k int) string { return `{"chunk_id":1,"k":` + strconv.Itoa(k) + `}` },
	},
	{
		name:      "dir2mcp_ask_audio",
		argsWithK: func(k int) string { return `{"question":"q","k":` + strconv.Itoa(k) + `}` },
	},
	{
		name: "dir2mcp_transcribe_and_ask",
		argsWithK: func(k int) string {
			return `{"rel_path":"a.mp3","question":"q","k":` + strconv.Itoa(k) + `}`
		},
	},
}

// TestToolK_SuppliedOutOfBoundIsInvalidRange pins issue #648 across the shared
// k parser: every tool advertises "minimum": 1 / "maximum": 50 for k, so a
// SUPPLIED value outside that bound MUST be the machine-parseable INVALID_RANGE
// error. k=0 and k=-1 previously became the default k silently, so a caller got
// a retrieval it did not request and no way to detect the substitution.
//
// The in-bound rows (1 and 50) assert the converse: a valid k is never turned
// into a range error. They may still fail for another reason (no retriever is
// wired in a conformance server), which is why the assertion is on the code.
func TestToolK_SuppliedOutOfBoundIsInvalidRange(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	for _, tool := range kBearingTools {
		for _, tc := range []struct {
			k         int
			wantRange bool
		}{
			{k: -1, wantRange: true},
			{k: 0, wantRange: true},
			{k: 1, wantRange: false},
			{k: 50, wantRange: false},
			{k: 51, wantRange: true},
		} {
			t.Run(tool.name+"/k="+strconv.Itoa(tc.k), func(t *testing.T) {
				envelope := callToolRaw(t, mcpURL, sid, tool.name, tool.argsWithK(tc.k))
				code := envelopeErrorCode(envelope)
				if tc.wantRange && code != "INVALID_RANGE" {
					t.Fatalf("%s with k=%d: code=%q want INVALID_RANGE", tool.name, tc.k, code)
				}
				if !tc.wantRange && code == "INVALID_RANGE" {
					t.Fatalf("%s with in-bound k=%d must not be INVALID_RANGE", tool.name, tc.k)
				}
			})
		}
	}
}

// TestToolK_OmittedStillResolvesToDefault pins the other half of #648: making a
// supplied out-of-bound k an error must not make an OMITTED k an error. An
// absent k stays the shipped default, so an existing caller sees no change.
func TestToolK_OmittedStillResolvesToDefault(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	for _, tool := range kBearingTools {
		t.Run(tool.name, func(t *testing.T) {
			args := tool.argsWithK(1)
			// Drop the k member so the tool sees an omitted field.
			var decoded map[string]interface{}
			if err := json.Unmarshal([]byte(args), &decoded); err != nil {
				t.Fatalf("decode fixture args: %v", err)
			}
			delete(decoded, "k")
			withoutK, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("encode fixture args: %v", err)
			}
			envelope := callToolRaw(t, mcpURL, sid, tool.name, string(withoutK))
			if code := envelopeErrorCode(envelope); code == "INVALID_RANGE" {
				t.Fatalf("%s with omitted k must not be INVALID_RANGE", tool.name)
			}
		})
	}
}
