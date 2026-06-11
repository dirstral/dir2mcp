package tests

import (
	"os"
	"path/filepath"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/secrets"
)

// chdir switches to dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func resolvedEmbedKey(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	prof, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	return prof.APIKey
}

// Keychain supplies the credential when neither the environment nor .env.local
// has it (SPEC §16.1.1 source #2).
func TestKeychainFillsWhenEnvAndFileAbsent(t *testing.T) {
	keyring.MockInit()
	chdir(t, t.TempDir())
	t.Setenv("MISTRAL_API_KEY", "")
	if err := secrets.Set(secrets.DefaultService, "MISTRAL_API_KEY", "kc-val"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	if got := resolvedEmbedKey(t); got != "kc-val" {
		t.Fatalf("embed key=%q want kc-val (from keychain)", got)
	}
}

// An explicit environment variable wins over the keychain (precedence #1 > #2).
func TestEnvWinsOverKeychain(t *testing.T) {
	keyring.MockInit()
	chdir(t, t.TempDir())
	t.Setenv("MISTRAL_API_KEY", "env-val")
	if err := secrets.Set(secrets.DefaultService, "MISTRAL_API_KEY", "kc-val"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	if got := resolvedEmbedKey(t); got != "env-val" {
		t.Fatalf("embed key=%q want env-val (env beats keychain)", got)
	}
}

// The keychain wins over .env.local (precedence #2 > #3).
func TestKeychainWinsOverDotEnvLocal(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	chdir(t, dir)
	t.Setenv("MISTRAL_API_KEY", "")
	if err := os.WriteFile(filepath.Join(dir, ".env.local"), []byte("MISTRAL_API_KEY=file-val\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	if err := secrets.Set(secrets.DefaultService, "MISTRAL_API_KEY", "kc-val"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	if got := resolvedEmbedKey(t); got != "kc-val" {
		t.Fatalf("embed key=%q want kc-val (keychain beats .env.local)", got)
	}
}

// DIR2MCP_DISABLE_KEYCHAIN skips the keychain entirely, falling through to file.
func TestDisableEnvVarSkipsKeychain(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	chdir(t, dir)
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv(secrets.DisableEnvVar, "1")
	if err := os.WriteFile(filepath.Join(dir, ".env.local"), []byte("MISTRAL_API_KEY=file-val\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	if err := secrets.Set(secrets.DefaultService, "MISTRAL_API_KEY", "kc-val"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	if got := resolvedEmbedKey(t); got != "file-val" {
		t.Fatalf("embed key=%q want file-val (keychain disabled)", got)
	}
}
