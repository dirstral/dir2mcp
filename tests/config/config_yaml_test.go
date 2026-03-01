package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/tests/testutil"
)

func TestLoadFile_ReadsYAMLValues(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"state_dir: ./repo/.state\n"+
		"listen_addr: 127.0.0.1:7000\n"+
		"mcp_path: /custom\n"+
		"public: true\n"+
		"auth_mode: none\n"+
		"allowed_origins:\n"+
		"  - https://example.com\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RootDir != "./repo" {
		t.Fatalf("RootDir=%q want=%q", cfg.RootDir, "./repo")
	}
	if cfg.StateDir != "./repo/.state" {
		t.Fatalf("StateDir=%q want=%q", cfg.StateDir, "./repo/.state")
	}
	if cfg.ListenAddr != "127.0.0.1:7000" {
		t.Fatalf("ListenAddr=%q want=%q", cfg.ListenAddr, "127.0.0.1:7000")
	}
	if cfg.MCPPath != "/custom" {
		t.Fatalf("MCPPath=%q want=%q", cfg.MCPPath, "/custom")
	}
	if !cfg.Public {
		t.Fatal("expected Public=true")
	}
	if cfg.AuthMode != "none" {
		t.Fatalf("AuthMode=%q want=%q", cfg.AuthMode, "none")
	}
	if len(cfg.AllowedOrigins) != 1 || cfg.AllowedOrigins[0] != "https://example.com" {
		t.Fatalf("AllowedOrigins=%v want=[https://example.com]", cfg.AllowedOrigins)
	}
}

func TestLoad_FileThenEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"mistral_base_url: https://yaml.example\n"+
		"embed_model_text: yaml-embed\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("MISTRAL_BASE_URL", "https://env.example")
		t.Setenv("DIR2MCP_EMBED_MODEL_TEXT", "env-embed")

		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.MistralBaseURL != "https://env.example" {
			t.Fatalf("MistralBaseURL=%q want=%q", cfg.MistralBaseURL, "https://env.example")
		}
		if cfg.EmbedModelText != "env-embed" {
			t.Fatalf("EmbedModelText=%q want=%q", cfg.EmbedModelText, "env-embed")
		}
	})
}

func TestSaveFile_WritesNonSecretYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MistralAPIKey = "super-secret"
	cfg.ElevenLabsAPIKey = "another-secret"
	cfg.AllowedOrigins = []string{"http://localhost", "https://example.com"}

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "root_dir: /tmp/repo") {
		t.Fatalf("saved yaml missing root_dir, got:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "mistral_api_key") {
		t.Fatalf("saved yaml must not include MISTRAL_API_KEY key, got:\n%s", text)
	}
	if strings.Contains(text, "super-secret") || strings.Contains(text, "another-secret") {
		t.Fatalf("saved yaml must not include secret values, got:\n%s", text)
	}
}

func TestSaveFile_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "negative-inactivity",
			cfg: func() config.Config {
				c := config.Default()
				c.SessionInactivityTimeout = -1 * time.Second
				return c
			}(),
		},
		{
			name: "max-lifetime-shorter-than-inactivity",
			cfg: func() config.Config {
				c := config.Default()
				c.SessionInactivityTimeout = 10 * time.Minute
				c.SessionMaxLifetime = 5 * time.Minute
				return c
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, ".dir2mcp.yaml")
			cfg := tc.cfg
			if err := config.SaveFile(path, cfg); err == nil {
				t.Fatal("expected error saving invalid config, got nil")
			} else if !strings.Contains(err.Error(), "validate config") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadFile_MalformedYAMLReturnsError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: [unterminated\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("expected LoadFile to fail on malformed YAML")
	}
	if !strings.Contains(err.Error(), "parse config file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadFile_ReadsNestedSpecStyleKeys(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"rag:\n"+
		"  system_prompt: use citations\\nline2\n"+
		"  max_context_chars: 12345\n"+
		"  oversample_factor: 7\n"+
		"ingest:\n"+
		"  gitignore: false\n"+
		"  follow_symlinks: true\n"+
		"  max_file_mb: 42\n"+
		"stt:\n"+
		"  provider: elevenlabs\n"+
		"  mistral:\n"+
		"    model: voxtral-large-latest\n"+
		"  elevenlabs:\n"+
		"    model: scribe_v2\n"+
		"    language_code: tr\n"+
		"server:\n"+
		"  tls:\n"+
		"    cert_file: /tmp/cert.pem\n"+
		"    key_file: /tmp/key.pem\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RAGSystemPrompt != "use citations\nline2" {
		t.Fatalf("RAGSystemPrompt=%q", cfg.RAGSystemPrompt)
	}
	if cfg.RAGMaxContextChars != 12345 {
		t.Fatalf("RAGMaxContextChars=%d", cfg.RAGMaxContextChars)
	}
	if cfg.RAGOversampleFactor != 7 {
		t.Fatalf("RAGOversampleFactor=%d", cfg.RAGOversampleFactor)
	}
	if cfg.IngestGitignore {
		t.Fatal("expected IngestGitignore=false")
	}
	if !cfg.IngestFollowSymlinks {
		t.Fatal("expected IngestFollowSymlinks=true")
	}
	if cfg.IngestMaxFileMB != 42 {
		t.Fatalf("IngestMaxFileMB=%d", cfg.IngestMaxFileMB)
	}
	if cfg.STTProvider != "elevenlabs" {
		t.Fatalf("STTProvider=%q", cfg.STTProvider)
	}
	if cfg.STTMistralModel != "voxtral-large-latest" {
		t.Fatalf("STTMistralModel=%q", cfg.STTMistralModel)
	}
	if cfg.STTElevenLabsModel != "scribe_v2" {
		t.Fatalf("STTElevenLabsModel=%q", cfg.STTElevenLabsModel)
	}
	if cfg.STTElevenLabsLanguageCode != "tr" {
		t.Fatalf("STTElevenLabsLanguageCode=%q", cfg.STTElevenLabsLanguageCode)
	}
	if cfg.ServerTLSCertFile != "/tmp/cert.pem" {
		t.Fatalf("ServerTLSCertFile=%q", cfg.ServerTLSCertFile)
	}
	if cfg.ServerTLSKeyFile != "/tmp/key.pem" {
		t.Fatalf("ServerTLSKeyFile=%q", cfg.ServerTLSKeyFile)
	}
}

func TestLoadFile_FlatAliasesRemainSupportedForNestedFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"rag_system_prompt: flat prompt\n"+
		"max_context_chars: 999\n"+
		"oversample_factor: 3\n"+
		"ingest_gitignore: false\n"+
		"follow_symlinks: true\n"+
		"max_file_mb: 10\n"+
		"stt_provider: mistral\n"+
		"stt_mistral_model: voxtral-mini-latest\n"+
		"stt_elevenlabs_model: scribe\n"+
		"elevenlabs_language_code: en\n"+
		"tls_cert_file: /etc/cert.pem\n"+
		"tls_key_file: /etc/key.pem\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RAGSystemPrompt != "flat prompt" || cfg.RAGMaxContextChars != 999 || cfg.RAGOversampleFactor != 3 {
		t.Fatalf("rag aliases not applied: %+v", cfg)
	}
	if cfg.IngestGitignore || !cfg.IngestFollowSymlinks || cfg.IngestMaxFileMB != 10 {
		t.Fatalf("ingest aliases not applied: gitignore=%v follow=%v max=%d", cfg.IngestGitignore, cfg.IngestFollowSymlinks, cfg.IngestMaxFileMB)
	}
	if cfg.STTProvider != "mistral" || cfg.STTMistralModel != "voxtral-mini-latest" || cfg.STTElevenLabsModel != "scribe" || cfg.STTElevenLabsLanguageCode != "en" {
		t.Fatalf("stt aliases not applied: %+v", cfg)
	}
	if cfg.ServerTLSCertFile != "/etc/cert.pem" || cfg.ServerTLSKeyFile != "/etc/key.pem" {
		t.Fatalf("tls aliases not applied: cert=%q key=%q", cfg.ServerTLSCertFile, cfg.ServerTLSKeyFile)
	}
}

func TestLoadFile_LastValueWinsAcrossAliasForms(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"rag_max_context_chars: 100\n"+
		"rag:\n"+
		"  max_context_chars: 200\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RAGMaxContextChars != 200 {
		t.Fatalf("RAGMaxContextChars=%d want=200", cfg.RAGMaxContextChars)
	}
}
