package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestLoadFile_AcceptsAUTF8BOM pins that a config file with a UTF-8 byte order
// mark loads fully. Windows PowerShell 5.1 writes one with
// `Set-Content -Encoding utf8`. Before the fix the BOM became part of the first
// key, and that key was dropped with no error.
func TestLoadFile_AcceptsAUTF8BOM(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	body := bom + "providers:\n  local:\n    kind: openai\n    base_url: http://127.0.0.1:11434/v1\n" +
		"    embed_text_model: nomic-embed-text\n    chat_model: qwen2.5:7b\n" +
		"model:\n  embed: {provider: local}\n  chat: {provider: local}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	prof, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil || prof.Name != "local" {
		t.Fatalf("embed resolves to %q (err %v), want the local profile from the first key", prof.Name, err)
	}

	flat := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(flat, []byte(bom+"root_dir: /data/bom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := config.LoadFile(flat); err != nil || cfg.RootDir != "/data/bom" {
		t.Fatalf("flat first key: root_dir=%q err=%v, want /data/bom", cfg.RootDir, err)
	}
}
