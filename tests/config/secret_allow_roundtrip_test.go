package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSecretAllowPatterns_DefaultRoundTrips pins that the default allow
// pattern, which holds a single quote, a double quote and brackets, survives
// the effective-config snapshot and a config file written as the docs show
// (single-quoted YAML, the single quote doubled).
func TestSecretAllowPatterns_DefaultRoundTrips(t *testing.T) {
	want := config.DefaultSecretAllowPatterns()

	cfg := config.Default()
	cfg.RootDir = testutil.TempDir(t)
	cfg.StateDir = testutil.TempDir(t)
	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !reflect.DeepEqual(loaded.SecretAllowPatterns, want) {
		t.Fatalf("snapshot round trip = %q, want %q", loaded.SecretAllowPatterns, want)
	}

	yaml := "security:\n  secret_allow_patterns:\n" +
		`    - '(?i)(?:^|[\s"''<>(\[])https?://(?:www\.|m\.)?youtube\.com/redirect\?(?:[^\s&#<>"'']*&)*?(redir_token=[^\s&#;?=:<>"'']*)'` + "\n"
	file := filepath.Join(testutil.TempDir(t), ".dir2mcp.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := config.LoadFile(file)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(fromFile.SecretAllowPatterns, want) {
		t.Fatalf("config file = %q, want %q", fromFile.SecretAllowPatterns, want)
	}
}
