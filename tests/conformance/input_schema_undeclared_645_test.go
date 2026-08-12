package conformance

import (
	"encoding/json"
	"sort"
	"testing"
)

// propertySchemas returns the "properties" object of an input schema, or nil when
// the tool takes no arguments.
func propertySchemas(schema map[string]interface{}) map[string]interface{} {
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	return properties
}

// probeVocabulary collects every property name any tool advertises, with one
// sample schema per name so a probe value is type-plausible for that name.
func probeVocabulary(schemas map[string]map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, schema := range schemas {
		for name, raw := range propertySchemas(schema) {
			prop, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if _, seen := out[name]; !seen {
				out[name] = prop
			}
		}
	}
	return out
}

// undeclaredProbes returns the probe names a tool does NOT advertise, sorted.
func undeclaredProbes(schema map[string]interface{}, vocabulary map[string]map[string]interface{}) []string {
	declared := propertySchemas(schema)
	probes := make([]string, 0, len(vocabulary))
	for name := range vocabulary {
		if _, ok := declared[name]; !ok {
			probes = append(probes, name)
		}
	}
	sort.Strings(probes)
	return probes
}

// requiredSampleArgs builds the minimal valid argument object for a tool: one
// sample value per required property (including a oneOf branch's requirements).
func requiredSampleArgs(schema map[string]interface{}) map[string]interface{} {
	args := map[string]interface{}{}
	declared := propertySchemas(schema)
	for _, name := range requiredPropertyNames(schema) {
		if prop, ok := declared[name].(map[string]interface{}); ok {
			args[name] = sampleValueForSchema(prop)
		}
	}
	return args
}

// sortedToolNames returns the advertised tool names in a stable order.
func sortedToolNames(schemas map[string]map[string]interface{}) []string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestInputSchema_UndeclaredPropertyIsRejected is the reverse guard of
// TestInputSchema_EveryAdvertisedPropertyIsAccepted, and the contract test issue
// #645 asks for.
//
// Every tool's advertised schema sets additionalProperties:false, so a strict
// client refuses to send a field the schema does not declare. A handler that
// nevertheless honors such a field creates a hidden feature: the server and its
// own tests treat it as supported while no schema-driven client can ever reach
// it. #645 reported exactly that for dir2mcp_related and language_match.
//
// A conformance test cannot enumerate every possible name, so it probes with the
// names the OTHER tools advertise: a tool must report any property outside its
// own schema as an unknown argument. That covers the reported case (related does
// not advertise language_match, so it must refuse it) and every future one where
// two sibling tools drift apart.
func TestInputSchema_UndeclaredPropertyIsRejected(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	schemas := toolInputSchemas(t, mcpURL, sid)
	vocabulary := probeVocabulary(schemas)
	if len(vocabulary) == 0 {
		t.Fatal("no advertised input properties to probe with")
	}

	for _, toolName := range sortedToolNames(schemas) {
		schema := schemas[toolName]
		for _, probe := range undeclaredProbes(schema, vocabulary) {
			t.Run(toolName+"/"+probe, func(t *testing.T) {
				args := requiredSampleArgs(schema)
				args[probe] = sampleValueForSchema(vocabulary[probe])
				encoded, err := json.Marshal(args)
				if err != nil {
					t.Fatalf("encode arguments: %v", err)
				}
				envelope := callToolRaw(t, mcpURL, sid, toolName, string(encoded))
				assertUnknownArgument(t, toolName, probe, envelope)
			})
		}
	}
}

// assertUnknownArgument requires that a tools/call rejected the named property as
// an unknown argument (INVALID_FIELD, df-008).
func assertUnknownArgument(t *testing.T, toolName, property string, envelope toolCallResultEnvelope) {
	t.Helper()
	errObj := envelope.Result.StructuredContent.Error
	if errObj == nil {
		t.Fatalf("%s does not advertise %q but accepted it: a schema-driven client can never use it", toolName, property)
	}
	if errObj.Code != "INVALID_FIELD" || errObj.Message != "unknown argument: "+property {
		t.Fatalf("%s does not advertise %q; want INVALID_FIELD 'unknown argument', got code=%q message=%q",
			toolName, property, errObj.Code, errObj.Message)
	}
}
