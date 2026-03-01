package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dir2mcp/internal/config"
)

func TestSaveEffectiveSnapshot_RedactsSecretsAndPersistsSourceMetadata(t *testing.T) {
	stateDir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = stateDir
	cfg.MistralAPIKey = "mistral-secret"
	cfg.ElevenLabsAPIKey = "elevenlabs-secret"
	cfg.X402.FacilitatorToken = "x402-secret"
	cfg.ResolvedAuthToken = "auth-secret"
	cfg.RAGMaxContextChars = 777
	cfg.ServerTLSCertFile = "/tls/cert.pem"

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{
		MistralAPIKey:        "env",
		ElevenLabsAPIKey:     "keychain",
		X402FacilitatorToken: "file",
		AuthToken:            "session",
	})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot failed: %v", err)
	}

	if path != filepath.Join(stateDir, config.EffectiveConfigSnapshotFile) {
		t.Fatalf("snapshot path=%q", path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(raw)

	if strings.Contains(text, "mistral-secret") || strings.Contains(text, "elevenlabs-secret") || strings.Contains(text, "x402-secret") || strings.Contains(text, "auth-secret") {
		t.Fatalf("snapshot contains plaintext secret:\n%s", text)
	}
	if !strings.Contains(text, "secret_sources:") {
		t.Fatalf("snapshot missing secret_sources block:\n%s", text)
	}
	if !strings.Contains(text, "mistral_api_key: env") || !strings.Contains(text, "auth_token: session") {
		t.Fatalf("snapshot missing expected source metadata:\n%s", text)
	}

	loadedCfg, loadedSources, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot failed: %v", err)
	}
	if loadedCfg.RAGMaxContextChars != 777 {
		t.Fatalf("RAGMaxContextChars=%d", loadedCfg.RAGMaxContextChars)
	}
	if loadedCfg.ServerTLSCertFile != "/tls/cert.pem" {
		t.Fatalf("ServerTLSCertFile=%q", loadedCfg.ServerTLSCertFile)
	}
	if loadedCfg.MistralAPIKey != "" || loadedCfg.ElevenLabsAPIKey != "" || loadedCfg.X402.FacilitatorToken != "" || loadedCfg.ResolvedAuthToken != "" {
		t.Fatalf("loaded snapshot should not hydrate secret values")
	}
	if loadedSources.MistralAPIKey != "env" || loadedSources.ElevenLabsAPIKey != "keychain" || loadedSources.X402FacilitatorToken != "file" || loadedSources.AuthToken != "session" {
		t.Fatalf("unexpected loaded sources: %+v", loadedSources)
	}
}

func TestLoadEffectiveSnapshot_ReadsNestedSecretSourceMetadata(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, config.EffectiveConfigSnapshotFile)
	writeFile(t, path, ""+
		"state_dir: /tmp/state\n"+
		"rag.max_context_chars: 4321\n"+
		"secret_sources:\n"+
		"  mistral_api_key: env\n"+
		"  elevenlabs_api_key: file\n")

	cfg, sources, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot failed: %v", err)
	}
	if cfg.RAGMaxContextChars != 4321 {
		t.Fatalf("RAGMaxContextChars=%d", cfg.RAGMaxContextChars)
	}
	if sources.MistralAPIKey != "env" || sources.ElevenLabsAPIKey != "file" {
		t.Fatalf("unexpected source metadata: %+v", sources)
	}
}
