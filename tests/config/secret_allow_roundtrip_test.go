package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

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

// TestSnapshotLists_RoundTripEveryCharacter pins that a list value survives the
// effective-config snapshot byte for byte. A value that begins and ends with a
// quote character used to lose those quotes on reload: the allow pattern
// 'token=[A-Z]{20}' (with literal quotes) became token=[A-Z]{20}, which
// cancels more.
func TestSnapshotLists_RoundTripEveryCharacter(t *testing.T) {
	secret := []string{
		`'token=[A-Z]{20}'`,
		`"quoted-[0-9]{4}"`,
		`MYCORP-[0-9]{4,8}`,
		`key # not a comment`,
		`it's`,
		`[a, b]`,
		`{x: 1}`,
		`- dash`,
		`a: b`,
		`back\slash\d+`,
		`null`,
		`123`,
	}
	excludes := []string{`*.md`, `**/.git/**`, `docs/build`, `my dir/notes`, `it's/here`}
	allow := []string{
		`'token=[A-Z]{20}'`,
		`''double-single''`,
		`ctx (feature_token=[A-Za-z0-9]+), more`,
	}
	cfg := config.Default()
	cfg.RootDir = testutil.TempDir(t)
	cfg.StateDir = testutil.TempDir(t)
	cfg.SecretPatterns = append([]string(nil), secret...)
	cfg.SecretAllowPatterns = append([]string(nil), allow...)
	cfg.PathExcludes = append([]string(nil), excludes...)
	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !reflect.DeepEqual(loaded.PathExcludes, excludes) {
		t.Errorf("path_excludes round trip:\n got %q\nwant %q", loaded.PathExcludes, excludes)
	}
	// The snapshot is also valid YAML for a standard parser, which the
	// secret-policy provenance uses.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Secret []string `yaml:"secret_patterns"`
		Allow  []string `yaml:"secret_allow_patterns"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the snapshot is not valid YAML: %v", err)
	}
	if !reflect.DeepEqual(parsed.Secret, secret) || !reflect.DeepEqual(parsed.Allow, allow) {
		t.Errorf("a standard YAML parser reads other lists:\n got %q / %q", parsed.Secret, parsed.Allow)
	}
	if !reflect.DeepEqual(loaded.SecretPatterns, secret) {
		t.Errorf("secret_patterns round trip:\n got %q\nwant %q", loaded.SecretPatterns, secret)
	}
	if !reflect.DeepEqual(loaded.SecretAllowPatterns, allow) {
		t.Errorf("secret_allow_patterns round trip:\n got %q\nwant %q", loaded.SecretAllowPatterns, allow)
	}
}
