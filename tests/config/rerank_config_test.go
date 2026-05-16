package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

func rerankBoolPtr(b bool) *bool { return &b }

func TestRerank_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"rerank:",
		"  enabled: true",
		"  provider: cohere",
		"  candidate_pool: 25",
		"  cohere:",
		"    api_key: yaml-cohere-secret",
		"    model: rerank-v3.5",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RerankEnabled == nil || !*cfg.RerankEnabled {
		t.Fatalf("RerankEnabled=%v, want explicit true", cfg.RerankEnabled)
	}
	if cfg.RerankProvider != "cohere" {
		t.Fatalf("RerankProvider=%q, want cohere", cfg.RerankProvider)
	}
	if cfg.RerankCandidatePool != 25 {
		t.Fatalf("RerankCandidatePool=%d, want 25", cfg.RerankCandidatePool)
	}
	if cfg.RerankModel != "rerank-v3.5" {
		t.Fatalf("RerankModel=%q, want rerank-v3.5", cfg.RerankModel)
	}
	if cfg.CohereAPIKey != "yaml-cohere-secret" {
		t.Fatalf("CohereAPIKey=%q, want yaml-cohere-secret", cfg.CohereAPIKey)
	}
}

func TestRerank_CohereBaseURLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"rerank:",
		"  cohere:",
		"    base_url: https://cohere.internal.example",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.CohereBaseURL != "https://cohere.internal.example" {
		t.Fatalf("CohereBaseURL=%q, want https://cohere.internal.example", cfg.CohereBaseURL)
	}

	// COHERE_BASE_URL env wins; base URL is NOT a secret -> persisted.
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("COHERE_BASE_URL", "https://env.cohere.example")
		c, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.CohereBaseURL != "https://env.cohere.example" {
			t.Fatalf("env override failed; got %q", c.CohereBaseURL)
		}
		c.StateDir = t.TempDir()
		snap, err := config.SaveEffectiveSnapshot(c, config.SecretSourceMetadata{})
		if err != nil {
			t.Fatalf("SaveEffectiveSnapshot: %v", err)
		}
		raw, err := os.ReadFile(snap)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), "cohere_base_url: https://env.cohere.example") {
			t.Fatalf("base URL must persist (not a secret):\n%s", raw)
		}
	})
}

func TestRerank_DefaultsWhenUnset(t *testing.T) {
	cfg := config.Default()
	if cfg.RerankEnabled != nil {
		t.Fatalf("rerank.enabled must be unset (auto) by default, got %v", *cfg.RerankEnabled)
	}
	if cfg.RerankProvider != "cohere" {
		t.Fatalf("default provider=%q, want cohere", cfg.RerankProvider)
	}
	if cfg.RerankModel != "rerank-v3.5" {
		t.Fatalf("default model=%q, want rerank-v3.5", cfg.RerankModel)
	}
	if cfg.RerankCandidatePool != 50 {
		t.Fatalf("default candidate_pool=%d, want 50", cfg.RerankCandidatePool)
	}
}

func TestRerank_EnvOverrides(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "rerank:\n  enabled: false\n  cohere:\n    api_key: yaml-key\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("COHERE_API_KEY", "env-cohere-secret")
		t.Setenv("DIR2MCP_RERANK_ENABLED", "true")
		t.Setenv("DIR2MCP_RERANK_MODEL", "rerank-english-v3.0")
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CohereAPIKey != "env-cohere-secret" {
			t.Fatalf("env must win for key; got %q", cfg.CohereAPIKey)
		}
		if cfg.RerankEnabled == nil || !*cfg.RerankEnabled {
			t.Fatalf("DIR2MCP_RERANK_ENABLED=true must set explicit true, got %v", cfg.RerankEnabled)
		}
		if cfg.RerankModel != "rerank-english-v3.0" {
			t.Fatalf("DIR2MCP_RERANK_MODEL override failed; got %q", cfg.RerankModel)
		}
	})
}

func TestRerank_CohereKeyNeverPersisted(t *testing.T) {
	stateDir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = stateDir
	cfg.RerankEnabled = rerankBoolPtr(true)
	cfg.CohereAPIKey = "cohere-plaintext-secret"

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{
		CohereAPIKey: "env",
	})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "cohere-plaintext-secret") {
		t.Fatalf("snapshot leaked the Cohere API key:\n%s", text)
	}
	if !strings.Contains(text, "cohere_api_key: env") {
		t.Fatalf("snapshot missing cohere secret-source metadata:\n%s", text)
	}
	// Non-secret rerank fields SHOULD persist.
	if !strings.Contains(text, "rerank_enabled: true") {
		t.Fatalf("snapshot missing rerank_enabled:\n%s", text)
	}

	loaded, sources, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if loaded.CohereAPIKey != "" {
		t.Fatalf("loaded snapshot must not carry the key, got %q", loaded.CohereAPIKey)
	}
	if sources.CohereAPIKey != "env" {
		t.Fatalf("secret source metadata not round-tripped; got %q", sources.CohereAPIKey)
	}
}

// TestRerank_EffectiveResolution pins the tri-state -> effective rule
// used for the persisted snapshot: explicit value wins; when unset
// (auto), reranking is on iff a credential is present.
func TestRerank_EffectiveResolution(t *testing.T) {
	cases := []struct {
		name    string
		enabled *bool
		key     string
		want    string
	}{
		{"auto+credential -> on", nil, "cohere-secret", "rerank_enabled: true"},
		{"auto+no-credential -> off", nil, "", "rerank_enabled: false"},
		{"explicit false beats credential", rerankBoolPtr(false), "cohere-secret", "rerank_enabled: false"},
		{"explicit true without credential", rerankBoolPtr(true), "", "rerank_enabled: true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StateDir = t.TempDir()
			cfg.RerankEnabled = tc.enabled
			cfg.CohereAPIKey = tc.key

			path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
			if err != nil {
				t.Fatalf("SaveEffectiveSnapshot: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("want %q in snapshot, got:\n%s", tc.want, raw)
			}
		})
	}
}
