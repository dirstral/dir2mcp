package conformance

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// kSchemaTools are the five tools whose input schema advertises k (SPEC §9.1
// scope: search, ask, related, ask_audio, transcribe_and_ask). rag.k_default is
// one setting about how much evidence a corpus needs, so a server MUST NOT
// apply it to some of them and not others.
var kSchemaTools = []string{
	"dir2mcp_search",
	"dir2mcp_ask",
	"dir2mcp_related",
	"dir2mcp_ask_audio",
	"dir2mcp_transcribe_and_ask",
}

// advertisedKProperty returns the decoded `k` property of a tool's served input
// schema, failing the test when the tool or the property is absent.
func advertisedKProperty(t *testing.T, schemas map[string]map[string]interface{}, tool string) map[string]interface{} {
	t.Helper()
	schema, ok := schemas[tool]
	if !ok {
		t.Fatalf("tools/list did not advertise %s", tool)
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: inputSchema has no properties object", tool)
	}
	prop, ok := properties["k"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: inputSchema does not advertise k: %#v", tool, properties["k"])
	}
	return prop
}

// TestToolsList_AdvertisedKDefaultIsTheConfiguredDefault pins the wire-visible
// half of issue #654. SPEC §9.1: "The `default` a server advertises for `k` MUST
// therefore be the value an omitted field actually produces, which is
// `rag.k_default` when the operator set one."
//
// A served schema describes THIS deployment. A fixed advertised default states
// something untrue the moment an operator configures another value: a client
// that reads the schema and sends the advertised number explicitly gets a
// different k from a client that omits the field, with nothing to explain the
// difference.
//
// The configuration under test fixes rag.k_default, which is what lets this
// fixture assert a specific advertised number at all (SPEC §9.1).
//
// On main every tool advertised the constant 15 whatever rag.k_default said, so
// all five rows fail there.
func TestToolsList_AdvertisedKDefaultIsTheConfiguredDefault(t *testing.T) {
	t.Parallel()
	const configuredK = 23

	cfg := defaultConfig()
	cfg.RAGKDefault = configuredK
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	schemas := toolInputSchemas(t, mcpURL, sid)
	for _, tool := range kSchemaTools {
		t.Run(tool, func(t *testing.T) {
			prop := advertisedKProperty(t, schemas, tool)
			got, ok := prop["default"].(float64)
			if !ok {
				t.Fatalf("%s: k advertises no numeric default: %#v", tool, prop["default"])
			}
			if int(got) != configuredK {
				t.Fatalf("%s: k default=%d want the configured rag.k_default=%d", tool, int(got), configuredK)
			}
		})
	}
}

// TestToolsList_AdvertisedKDefaultIsTheShippedFallback is the other half of the
// same rule: with no operator value, the advertised default is the shipped
// fallback. config.Default() ships that number as rag.k_default, so the config
// template, the served schema and the runtime all state one value.
func TestToolsList_AdvertisedKDefaultIsTheShippedFallback(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	schemas := toolInputSchemas(t, mcpURL, sid)
	for _, tool := range kSchemaTools {
		t.Run(tool, func(t *testing.T) {
			prop := advertisedKProperty(t, schemas, tool)
			got, ok := prop["default"].(float64)
			if !ok {
				t.Fatalf("%s: k advertises no numeric default: %#v", tool, prop["default"])
			}
			if int(got) != config.RAGKFallback {
				t.Fatalf("%s: k default=%d want the shipped fallback %d", tool, int(got), config.RAGKFallback)
			}
		})
	}
}

// TestToolsList_AdvertisedKKeepsItsBound guards the bound while the default
// moves: publishing an effective default must not disturb the 1..50 minimum and
// maximum every k-bearing tool advertises (#648), and the configured default
// must itself lie inside that bound (SPEC §9.1).
func TestToolsList_AdvertisedKKeepsItsBound(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.RAGKDefault = 42
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	schemas := toolInputSchemas(t, mcpURL, sid)
	for _, tool := range kSchemaTools {
		t.Run(tool, func(t *testing.T) {
			prop := advertisedKProperty(t, schemas, tool)
			min, minOK := prop["minimum"].(float64)
			max, maxOK := prop["maximum"].(float64)
			if !minOK || !maxOK {
				t.Fatalf("%s: k advertises no numeric bound: %#v", tool, prop)
			}
			if int(min) != config.RAGKMin || int(max) != config.RAGKMax {
				t.Fatalf("%s: k bound=%d..%d want %d..%d", tool, int(min), int(max), config.RAGKMin, config.RAGKMax)
			}
			got, ok := prop["default"].(float64)
			if !ok || got < min || got > max {
				t.Fatalf("%s: advertised default %#v is outside the advertised bound %d..%d", tool, prop["default"], int(min), int(max))
			}
		})
	}
}
