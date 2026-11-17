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

// TestInlineList_CommaInsideAQuotedItem pins that an inline list is split
// only at a comma outside a quoted scalar. A regular expression with `{20,}`
// in a quoted item stays one item, for every list key.
func TestInlineList_CommaInsideAQuotedItem(t *testing.T) {
	yaml := "security:\n" +
		`  secret_allow_patterns: ['https://example\.com/\?(page_token=[A-Z]{20,})', "feature_[a-z]{2,4}=x"]` + "\n" +
		`  secret_patterns: ['MYCORP-[0-9]{4,}', plain-[a-z]+, 'it''s, quoted']` + "\n"
	file := filepath.Join(testutil.TempDir(t), ".dir2mcp.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(file)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	wantAllow := []string{`https://example\.com/\?(page_token=[A-Z]{20,})`, `feature_[a-z]{2,4}=x`}
	if !reflect.DeepEqual(cfg.SecretAllowPatterns, wantAllow) {
		t.Errorf("secret_allow_patterns = %q, want %q", cfg.SecretAllowPatterns, wantAllow)
	}
	wantSecret := []string{`MYCORP-[0-9]{4,}`, `plain-[a-z]+`, `it's, quoted`}
	if !reflect.DeepEqual(cfg.SecretPatterns, wantSecret) {
		t.Errorf("secret_patterns = %q, want %q", cfg.SecretPatterns, wantSecret)
	}
}
