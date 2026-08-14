package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Issue #841: the production transport refused a charset-qualified
// application/json POST with a plain-text 415 from inside the MCP SDK.
//
// The media type is what this server accepts or refuses. Parameters ride along.
// RFC 8259 §11 registers application/json with no charset parameter at all ("No
// 'charset' parameter is defined for this registration. Adding one really has no
// effect on compliant recipients."), so a parameter cannot make an acceptable
// media type unacceptable.
//
// These assertions run on the PRODUCTION transport (newServer starts
// mcp.SDKTransport on a real listener, the way internal/cli/up.go wires it), so
// they read the contract a real client gets.

// contentTypeCase is one Content-Type header spelling and the name it reports.
type contentTypeCase struct {
	name        string
	contentType string
}

// acceptedContentTypes all name the JSON media type. The bare spelling is the
// regression guard; the rest add the parameters, casing and whitespace RFC 9110
// §8.3 allows, and every one of them got a plain-text 415 before the #841 fix.
var acceptedContentTypes = []contentTypeCase{
	{"bare media type", "application/json"},
	{"charset parameter", "application/json; charset=utf-8"},
	{"upper case and no space", "Application/JSON;charset=UTF-8"},
	{"space before the parameter separator", "application/json ; charset=utf-8"},
	{"quoted parameter value", `application/json;charset="utf-8"`},
	{"unrelated parameter", "application/json; boundary=x"},
}

// refusedContentTypes name no media type this server serves. The fix must not
// loosen the check, so each one still gets a 415.
var refusedContentTypes = []contentTypeCase{
	{"wrong media type", "text/plain"},
	{"wrong media type with a parameter", "text/plain; charset=utf-8"},
	{"json subtype prefix only", "application/jsonx"},
	{"absent", ""},
	{"malformed parameter section", "application/json; charset"},
}

// postContentType sends one initialize POST with the given raw Content-Type
// header and returns the status, the response Content-Type and the body.
func postContentType(t *testing.T, url, contentType, body string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		// Send the header with an empty value, which is what a client that sets the
		// header but leaves it blank produces.
		req.Header["Content-Type"] = []string{""}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do POST: %v", err)
	}
	status := resp.StatusCode
	respCT := resp.Header.Get("Content-Type")
	return status, respCT, readBody(t, resp)
}

// canonicalErrorCode returns the canonical code carried in error.data.code.
func canonicalErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error *struct {
			Data *struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, body)
	}
	if envelope.Error == nil || envelope.Error.Data == nil {
		t.Fatalf("expected a canonical error envelope, body=%s", body)
	}
	return envelope.Error.Data.Code
}

// TestProduction_AcceptedContentTypesAreServed pins the accepted side of the
// contract: every spelling of the JSON media type reaches the handler and gets a
// JSON-RPC result.
func TestProduction_AcceptedContentTypesAreServed(t *testing.T) {
	t.Parallel()
	for _, tc := range acceptedContentTypes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultConfig()
			srv := newServer(t, cfg)
			defer srv.Close()

			status, _, body := postContentType(t, srv.URL+cfg.MCPPath, tc.contentType,
				`{"jsonrpc":"2.0","id":69,"method":"initialize","params":{}}`)
			if status != http.StatusOK || !hasResult(body) {
				t.Fatalf("Content-Type %q: status=%d body=%s, want 200 with a result", tc.contentType, status, body)
			}
		})
	}
}

// TestProduction_UnacceptableContentTypeGetsTheCanonicalEnvelope pins the
// refused side. The refusal is this server's canonical JSON-RPC envelope with
// the INVALID_FIELD code, the same contract every other transport-level refusal
// on this path uses (FORBIDDEN_ORIGIN, SESSION_NOT_FOUND, rate limiting), not the
// SDK's plain-text body a client cannot parse.
func TestProduction_UnacceptableContentTypeGetsTheCanonicalEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedContentTypes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultConfig()
			srv := newServer(t, cfg)
			defer srv.Close()

			status, respCT, body := postContentType(t, srv.URL+cfg.MCPPath, tc.contentType,
				`{"jsonrpc":"2.0","id":70,"method":"initialize","params":{}}`)
			if status != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q: status=%d body=%s, want 415", tc.contentType, status, body)
			}
			if !strings.HasPrefix(respCT, "application/json") {
				t.Fatalf("Content-Type %q: response Content-Type=%q, want application/json", tc.contentType, respCT)
			}
			if !hasError(body) {
				t.Fatalf("Content-Type %q: body=%s, want a JSON-RPC error envelope", tc.contentType, body)
			}
			if code := canonicalErrorCode(t, body); code != "INVALID_FIELD" {
				t.Fatalf("Content-Type %q: canonical code=%q, want INVALID_FIELD, body=%s", tc.contentType, code, body)
			}
			if rpcCode, _ := decodeRPCError(t, body); rpcCode != -32600 {
				t.Fatalf("Content-Type %q: json-rpc code=%d, want -32600, body=%s", tc.contentType, rpcCode, body)
			}
		})
	}
}

// TestParity_ContentTypeVerdict fails when the production transport and the
// direct handler disagree about which media type is acceptable. Issue #841 was
// two checks disagreeing, so both chains now read the same rule and this test
// keeps them together.
func TestParity_ContentTypeVerdict(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	production := newServer(t, cfg)
	defer production.Close()
	direct := newDirectHandlerServer(t, cfg)
	defer direct.Close()

	for _, tc := range acceptedContentTypes {
		assertParity(t, tc, cfg.MCPPath, production.URL, direct.URL, http.StatusOK)
	}
	for _, tc := range refusedContentTypes {
		assertParity(t, tc, cfg.MCPPath, production.URL, direct.URL, http.StatusUnsupportedMediaType)
	}
}

// assertParity checks that both chains answer one Content-Type spelling with the
// wanted status.
func assertParity(t *testing.T, tc contentTypeCase, mcpPath, productionURL, directURL string, want int) {
	t.Helper()
	const body = `{"jsonrpc":"2.0","id":71,"method":"initialize","params":{}}`
	prodStatus, _, prodBody := postContentType(t, productionURL+mcpPath, tc.contentType, body)
	directStatus, _, directBody := postContentType(t, directURL+mcpPath, tc.contentType, body)
	if prodStatus != want {
		t.Errorf("%s (%q): production status=%d want=%d body=%s", tc.name, tc.contentType, prodStatus, want, prodBody)
	}
	if directStatus != want {
		t.Errorf("%s (%q): direct status=%d want=%d body=%s", tc.name, tc.contentType, directStatus, want, directBody)
	}
}
