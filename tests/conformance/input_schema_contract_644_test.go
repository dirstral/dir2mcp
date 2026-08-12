package conformance

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// toolInputSchemas fetches tools/list and returns each tool's inputSchema as a
// decoded generic tree keyed by tool name.
func toolInputSchemas(t *testing.T, mcpURL, sessionID string) map[string]map[string]interface{} {
	t.Helper()
	resp := sendRPC(t, mcpURL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string                 `json:"name"`
				InputSchema map[string]interface{} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("tools/list: decode: %v body=%s", err, body)
	}
	out := make(map[string]map[string]interface{}, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		out[tool.Name] = tool.InputSchema
	}
	if len(out) == 0 {
		t.Fatal("tools/list advertised no tools")
	}
	return out
}

// sampleValueForSchema returns a value that satisfies the given property schema
// well enough to reach the handler's unknown-argument gate. It does not have to
// be semantically meaningful: the assertion below is only that the server does
// not answer "unknown argument", so a malformed date or an unknown path is a
// perfectly good sample.
func sampleValueForSchema(prop map[string]interface{}) interface{} {
	if enum, ok := prop["enum"].([]interface{}); ok && len(enum) > 0 {
		return enum[0]
	}
	switch prop["type"] {
	case "integer", "number":
		if min, ok := prop["minimum"].(float64); ok {
			return min
		}
		return 1
	case "boolean":
		return true
	case "array":
		return []interface{}{"x"}
	case "object":
		return map[string]interface{}{}
	default:
		return "x"
	}
}

// requiredPropertyNames returns the names a request must carry: the schema's own
// "required" list, plus the first "oneOf" branch's required list when the schema
// uses one (dir2mcp_related's chunk_id / rel_path choice).
func requiredPropertyNames(schema map[string]interface{}) []string {
	names := stringSliceFromJSON(schema["required"])
	if branches, ok := schema["oneOf"].([]interface{}); ok {
		for _, raw := range branches {
			branch, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if extra := stringSliceFromJSON(branch["required"]); len(extra) > 0 {
				names = append(names, extra...)
				break
			}
		}
	}
	return names
}

func stringSliceFromJSON(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestInputSchema_EveryAdvertisedPropertyIsAccepted is the anti-drift guard for
// the whole "the schema says one thing, the handler does another" defect class
// (issues #644 and #645).
//
// A schema-driven client reads inputSchema from tools/list and builds requests
// from it. Every property a tool advertises MUST therefore reach the handler:
// answering "unknown argument" to a field the server itself published leaves the
// client no way to use the contract it was given.
//
// The test walks every tool, sends one request per advertised property (plus the
// tool's required fields), and asserts the server never reports that property as
// unknown. Any other failure is fine: a conformance server has no retriever or
// corpus, so most calls fail for a reason that is not the contract.
//
// It fails on the pre-fix tree because dir2mcp_ask_audio advertised the whole
// inherited dir2mcp_ask filter set while its handler allowed only eight names.
func TestInputSchema_EveryAdvertisedPropertyIsAccepted(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	schemas := toolInputSchemas(t, mcpURL, sid)
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, toolName := range names {
		schema := schemas[toolName]
		properties, ok := schema["properties"].(map[string]interface{})
		if !ok {
			// A tool with no input properties (dir2mcp_stats) has nothing to check.
			continue
		}
		required := requiredPropertyNames(schema)
		propNames := make([]string, 0, len(properties))
		for prop := range properties {
			propNames = append(propNames, prop)
		}
		sort.Strings(propNames)

		for _, propName := range propNames {
			t.Run(toolName+"/"+propName, func(t *testing.T) {
				args := map[string]interface{}{}
				for _, name := range required {
					prop, ok := properties[name].(map[string]interface{})
					if !ok {
						continue
					}
					args[name] = sampleValueForSchema(prop)
				}
				prop, ok := properties[propName].(map[string]interface{})
				if !ok {
					t.Fatalf("%s: property %q is not an object schema: %#v", toolName, propName, properties[propName])
				}
				args[propName] = sampleValueForSchema(prop)

				encoded, err := json.Marshal(args)
				if err != nil {
					t.Fatalf("encode arguments: %v", err)
				}
				envelope := callToolRaw(t, mcpURL, sid, toolName, string(encoded))
				if envelope.Result.StructuredContent.Error == nil {
					return
				}
				message := envelope.Result.StructuredContent.Error.Message
				if strings.Contains(message, "unknown argument: "+propName) {
					t.Fatalf("%s advertises %q but its handler rejects it: %s", toolName, propName, message)
				}
			})
		}
	}
}
