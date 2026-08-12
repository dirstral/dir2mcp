package conformance

import (
	"encoding/json"
	"net/http"
	"testing"
)

// toolsListSchemas fetches tools/list and returns each tool's outputSchema as a
// decoded generic tree keyed by tool name.
func toolsListSchemas(t *testing.T) map[string]interface{} {
	t.Helper()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d want=200 body=%s", resp.StatusCode, body)
	}

	var envelope struct {
		Result struct {
			Tools []struct {
				Name         string                 `json:"name"`
				OutputSchema map[string]interface{} `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("tools/list: decode: %v body=%s", err, body)
	}
	out := make(map[string]interface{}, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		out[tool.Name] = tool.OutputSchema
	}
	return out
}

// findAllByKey walks a decoded JSON tree and returns every value found under the
// given key at any depth.
func findAllByKey(node interface{}, key string) []interface{} {
	var found []interface{}
	switch v := node.(type) {
	case map[string]interface{}:
		for k, child := range v {
			if k == key {
				found = append(found, child)
			}
			found = append(found, findAllByKey(child, key)...)
		}
	case []interface{}:
		for _, child := range v {
			found = append(found, findAllByKey(child, key)...)
		}
	}
	return found
}

// hasConstRegion reports whether the tree contains a span variant whose kind is
// the "region" const — i.e. the region span variant is advertised.
func hasConstRegion(node interface{}) bool {
	for _, kind := range findAllByKey(node, "kind") {
		if m, ok := kind.(map[string]interface{}); ok {
			if c, ok := m["const"].(string); ok && c == "region" {
				return true
			}
		}
	}
	return false
}

// TestOutputSchema_CoordOriginIsEnum asserts every bbox.coord_origin schema
// advertised across all tool outputSchemas constrains the value to the §5.4
// enum {TOPLEFT, BOTTOMLEFT} — not a bare {"type":"string"}, which a strict
// client would accept before the published Span rejects an out-of-enum value.
func TestOutputSchema_CoordOriginIsEnum(t *testing.T) {
	t.Parallel()
	schemas := toolsListSchemas(t)

	nodes := findAllByKey(schemas, "coord_origin")
	if len(nodes) == 0 {
		t.Fatal("no coord_origin schema found in any tool outputSchema")
	}
	for _, node := range nodes {
		m, ok := node.(map[string]interface{})
		if !ok {
			t.Fatalf("coord_origin schema is not an object: %#v", node)
		}
		if _, isString := m["type"]; isString {
			t.Fatalf("coord_origin must be enum-constrained, got type-based schema: %#v", m)
		}
		enum, ok := m["enum"].([]interface{})
		if !ok {
			t.Fatalf("coord_origin missing enum: %#v", m)
		}
		got := map[string]bool{}
		for _, e := range enum {
			if s, ok := e.(string); ok {
				got[s] = true
			}
		}
		if len(enum) != 2 || !got["TOPLEFT"] || !got["BOTTOMLEFT"] {
			t.Fatalf("coord_origin enum = %v, want exactly [TOPLEFT BOTTOMLEFT]", enum)
		}
	}
}

// TestOutputSchema_SearchAdvertisesRegionSpan asserts the search tool's
// outputSchema advertises the region span variant (§15.1.1), so a client can
// validate a region-provenance citation.
func TestOutputSchema_SearchAdvertisesRegionSpan(t *testing.T) {
	t.Parallel()
	schemas := toolsListSchemas(t)
	search, ok := schemas["dir2mcp_search"]
	if !ok {
		t.Fatal("dir2mcp_search not present in tools/list")
	}
	if !hasConstRegion(search) {
		t.Fatal("search outputSchema does not advertise a region span variant")
	}
}
