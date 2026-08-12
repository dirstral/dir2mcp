package tests

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// loadStatsModelsConfig writes a .dir2mcp.yaml, loads it, and returns a config
// ready to serve MCP. The providers subtree is only reachable through
// config.LoadFile, so a models-provenance test has to go through a file.
func loadStatsModelsConfig(t *testing.T, body string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	return cfg
}

// statsModelsFromServer calls dir2mcp_stats and returns the models block.
func statsModelsFromServer(t *testing.T, cfg config.Config) map[string]interface{} {
	t.Helper()
	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	t.Cleanup(srv.Close)
	sc := callStatsTool(t, srv.URL+cfg.MCPPath)
	models, ok := sc["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("models missing from stats payload: %#v", sc["models"])
	}
	return models
}

// TestMCPToolsCallStats_ModelsReportConfiguredIdentities pins issue #647: the
// models block MUST name the provider and model identities this deployment
// resolves, not the built-in Mistral constants.
//
// An operator who moves embeddings, OCR and STT onto other providers asks stats
// what produced the vectors and transcripts. The old handler answered
// "mistral-embed", "codestral-embed", "mistral-ocr-latest" and "mistral" for
// every deployment, so the provenance surface named a backend that is not in use.
func TestMCPToolsCallStats_ModelsReportConfiguredIdentities(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-test-key")
	t.Setenv("WHISPER_BASE_URL", "http://127.0.0.1:9/v1")
	t.Setenv("MISTRAL_API_KEY", "")

	cfg := loadStatsModelsConfig(t, ""+
		"stt_provider: whisper\n"+
		"providers:\n"+
		"  gemini:\n"+
		"    embed_text_model: gemini-embedding-001\n"+
		"    embed_code_model: gemini-embedding-001\n"+
		"    chat_model: gemini-2.5-pro\n"+
		"  house-ocr:\n"+
		"    kind: mistral\n"+
		"    base_url: http://127.0.0.1:9\n"+
		"    ocr_model: house-ocr-v3\n"+
		"  whisper:\n"+
		"    kind: whisper\n"+
		"    base_url: http://127.0.0.1:9/v1\n"+
		"    stt_model: large-v3-turbo\n"+
		"model:\n"+
		"  embed:\n"+
		"    provider: gemini\n"+
		"  chat:\n"+
		"    provider: gemini\n"+
		"  ocr:\n"+
		"    provider: house-ocr\n")

	models := statsModelsFromServer(t, cfg)
	for field, want := range map[string]string{
		"embed_text":   "gemini-embedding-001",
		"embed_code":   "gemini-embedding-001",
		"ocr":          "house-ocr-v3",
		"chat":         "gemini-2.5-pro",
		"stt_provider": "whisper",
		"stt_model":    "large-v3-turbo",
	} {
		if got, _ := models[field].(string); got != want {
			t.Errorf("models.%s = %q, want the configured %q", field, got, want)
		}
	}
}

// TestMCPToolsCallStats_ModelsFallBackWhenNothingResolves pins the other branch:
// with no eligible provider the block keeps the shipped defaults, so an
// unconfigured server still reports a full, required models object.
func TestMCPToolsCallStats_ModelsFallBackWhenNothingResolves(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("COHERE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ELEVENLABS_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")

	cfg := loadStatsModelsConfig(t, "root_dir: .\n")
	models := statsModelsFromServer(t, cfg)
	for _, field := range []string{"embed_text", "embed_code", "ocr", "stt_provider", "stt_model", "chat"} {
		if got, _ := models[field].(string); got == "" {
			t.Errorf("models.%s must stay populated when no provider resolves, got %#v", field, models[field])
		}
	}
}

// TestMCPToolsCallStats_STTProviderIsNotAClosedEnum pins the advertised half of
// #647. bs-007 and the canonical stats.json say stt_provider is "not a closed
// enum", and that any STT-capable provider is valid. The served schema pinned it
// to mistral|elevenlabs, so a strict client rejected the stats of a deployment
// that transcribes with whisper, openai, gemini or an operator-named profile.
func TestMCPToolsCallStats_STTProviderIsNotAClosedEnum(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	schema := toolOutputSchemaFromList(t, srv.URL+cfg.MCPPath, protocol.ToolNameStats)
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats outputSchema has no properties: %#v", schema)
	}
	models, ok := properties["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats outputSchema declares no models object: %v", keysOf(properties))
	}
	modelProps, ok := models["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("models schema has no properties: %#v", models)
	}
	sttProvider, ok := modelProps["stt_provider"].(map[string]interface{})
	if !ok {
		t.Fatalf("models schema declares no stt_provider: %v", keysOf(modelProps))
	}
	if enum, present := sttProvider["enum"]; present {
		t.Fatalf("stt_provider must not be a closed enum, got %#v", enum)
	}
}
