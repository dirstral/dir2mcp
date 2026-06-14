package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestValidate_QdrantRequiresURL asserts index.backend=qdrant without a URL is
// rejected with a clear, remediable error (issue #268).
func TestValidate_QdrantRequiresURL(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "qdrant"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "index.qdrant.url") {
		t.Fatalf("Validate(qdrant,no-url) = %v, want url-required error", err)
	}
}

// TestValidate_QdrantOK accepts a fully-specified qdrant backend and normalizes
// the backend value + trims url/collection.
func TestValidate_QdrantOK(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "QDRANT"
	cfg.Qdrant.URL = "  http://localhost:6334  "
	cfg.Qdrant.Collection = "  mycorpus  "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(valid qdrant): %v", err)
	}
	if cfg.IndexBackend != "qdrant" {
		t.Errorf("IndexBackend not normalized: got %q", cfg.IndexBackend)
	}
	if cfg.Qdrant.URL != "http://localhost:6334" {
		t.Errorf("Qdrant.URL not trimmed: got %q", cfg.Qdrant.URL)
	}
	if cfg.Qdrant.Collection != "mycorpus" {
		t.Errorf("Qdrant.Collection not trimmed: got %q", cfg.Qdrant.Collection)
	}
}

// TestValidate_QdrantURLNotRequiredForOtherBackends guards that the qdrant URL
// requirement only applies when the qdrant backend is selected.
func TestValidate_QdrantURLNotRequiredForOtherBackends(t *testing.T) {
	for _, backend := range []string{"memory", "disk"} {
		cfg := config.Default()
		cfg.IndexBackend = backend
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(backend=%s, no qdrant url) = %v, want nil", backend, err)
		}
	}
}

// TestValidate_UnknownIndexBackend rejects an unsupported backend and surfaces
// the full allowed set (now including qdrant).
func TestValidate_UnknownIndexBackend(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "elasticsearch"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "qdrant") {
		t.Fatalf("Validate(backend=elasticsearch) = %v, want allowed-set error listing qdrant", err)
	}
}

// TestLoadFile_QdrantNestedKeys checks the nested spec-style index.qdrant block
// parses into the flat Qdrant fields.
func TestLoadFile_QdrantNestedKeys(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"index_backend: qdrant",
		"index:",
		"  qdrant:",
		"    url: http://qdrant.local:6334",
		"    collection: docs",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(persisted qdrant block) = %v, want nil", err)
	}
	if cfg.IndexBackend != "qdrant" {
		t.Errorf("IndexBackend = %q, want qdrant", cfg.IndexBackend)
	}
	if cfg.Qdrant.URL != "http://qdrant.local:6334" {
		t.Errorf("Qdrant.URL = %q, want http://qdrant.local:6334", cfg.Qdrant.URL)
	}
	if cfg.Qdrant.Collection != "docs" {
		t.Errorf("Qdrant.Collection = %q, want docs", cfg.Qdrant.Collection)
	}
}

// TestSaveFile_QdrantRoundTrip guards the SaveFile -> LoadFile cycle for the
// non-secret qdrant fields.
func TestSaveFile_QdrantRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.IndexBackend = "qdrant"
	cfg.Qdrant.URL = "https://cluster.cloud.qdrant.io:6334"
	cfg.Qdrant.Collection = "persisted-collection"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.Qdrant.URL != "https://cluster.cloud.qdrant.io:6334" {
		t.Errorf("Qdrant.URL did not round-trip: got %q", loaded.Qdrant.URL)
	}
	if loaded.Qdrant.Collection != "persisted-collection" {
		t.Errorf("Qdrant.Collection did not round-trip: got %q", loaded.Qdrant.Collection)
	}
}

// TestSaveFile_NeverPersistsQdrantAPIKey asserts the resolved Qdrant api_key is
// never written to the config file (it is a runtime-only secret).
func TestSaveFile_NeverPersistsQdrantAPIKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.IndexBackend = "qdrant"
	cfg.Qdrant.URL = "http://localhost:6334"
	cfg.Qdrant.APIKey = "qdrant-topsecret-apikey"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw := string(rawBytes)
	if strings.Contains(raw, "qdrant-topsecret-apikey") {
		t.Fatalf("persisted config leaked qdrant api_key:\n%s", raw)
	}

	// The api_key must also not survive a reload (Save -> reload omits it).
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.Qdrant.APIKey != "" {
		t.Fatalf("reloaded Qdrant.APIKey = %q, want empty (secret must not persist)", loaded.Qdrant.APIKey)
	}
}

// TestLoad_QdrantAPIKeyFromEnv asserts the api_key resolves through the env
// secret precedence (QDRANT_API_KEY) on the env-aware Load path, while a
// file-supplied api_key is ignored (never read from disk).
func TestLoad_QdrantAPIKeyFromEnv(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"index_backend: qdrant",
		"index:",
		"  qdrant:",
		"    url: http://localhost:6334",
		"    api_key: file-supplied-should-be-ignored",
	}, "\n")+"\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	t.Setenv("QDRANT_API_KEY", "env-resolved-key")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Qdrant.APIKey != "env-resolved-key" {
		t.Fatalf("Qdrant.APIKey = %q, want env-resolved-key (file value must be ignored)", cfg.Qdrant.APIKey)
	}
}
