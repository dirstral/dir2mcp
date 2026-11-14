package tests

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// security.secret_allow_patterns (SPEC §7.2, §16.2): the default, an operator
// list that replaces it, and an invalid entry that fails the load.

func TestSecretAllowPatterns_Default(t *testing.T) {
	want := []string{`(?i)https?://(?:www\.|m\.)?youtube\.com/redirect\?(?:[^\s&#<>"]*&)*?redir_token=[^\s&#;?=:<>"]*`}
	if got := config.Default().SecretAllowPatterns; !reflect.DeepEqual(got, want) {
		t.Fatalf("default secret_allow_patterns = %q, want %q", got, want)
	}
	if got := config.DefaultSecretAllowPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSecretAllowPatterns() = %q, want %q", got, want)
	}
}

func TestSecretAllowPatterns_OperatorListReplacesTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"security:\n"+
		"  secret_allow_patterns:\n"+
		"    - 'feature_token=[A-Za-z0-9]+'\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if want := []string{`feature_token=[A-Za-z0-9]+`}; !reflect.DeepEqual(cfg.SecretAllowPatterns, want) {
		t.Fatalf("SecretAllowPatterns = %q, want %q", cfg.SecretAllowPatterns, want)
	}
}

func TestSecretAllowPatterns_InvalidEntryFailsTheLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"security:\n"+
		"  secret_allow_patterns:\n"+
		"    - '(unclosed'\n")
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile accepted an allow pattern that does not compile")
	}
	if !strings.Contains(err.Error(), "security.secret_allow_patterns") {
		t.Fatalf("error %q does not name security.secret_allow_patterns", err)
	}
}

func TestSecretPatterns_InvalidEntryFailsTheLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"security:\n"+
		"  secret_patterns:\n"+
		"    - '[unclosed'\n")
	_, err := config.LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "security.secret_patterns") {
		t.Fatalf("err = %v, want an error that names security.secret_patterns", err)
	}
}
