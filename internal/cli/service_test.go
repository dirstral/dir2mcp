package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceLabel(t *testing.T) {
	got := serviceLabel("dir2mcp-stas-legal-main-45a264")
	want := "com.dirstral.dir2mcp-stas-legal-main-45a264"
	if got != want {
		t.Fatalf("serviceLabel = %q, want %q", got, want)
	}
	if got := serviceLabel("  spaced  "); got != "com.dirstral.spaced" {
		t.Errorf("serviceLabel did not trim: %q", got)
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	spec := serviceSpec{
		Label:      "com.dirstral.dir2mcp-demo-abc123",
		BinaryPath: "/usr/local/bin/dir2mcp",
		WorkingDir: "/Users/me/legal & co",
		Args:       []string{"up", "--foreground"},
		LogPath:    "/Users/me/legal & co/.dir2mcp/service.log",
	}
	out := renderLaunchdPlist(spec)

	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.dirstral.dir2mcp-demo-abc123</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/dir2mcp</string>",
		"<string>up</string>",
		"<string>--foreground</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<true/>",
		"<key>WorkingDirectory</key>",
		"<key>ProcessType</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n---\n%s", want, out)
		}
	}

	// WorkingDirectory carries an ampersand: it must be XML-escaped so
	// the plist stays well-formed.
	if !strings.Contains(out, "legal &amp; co") {
		t.Errorf("plist did not XML-escape working dir:\n%s", out)
	}
	if strings.Contains(out, "legal & co") {
		t.Errorf("plist contains a raw unescaped ampersand:\n%s", out)
	}
	if !strings.HasPrefix(out, "<?xml") {
		t.Errorf("plist missing XML header: %q", out[:min(20, len(out))])
	}
}

func TestPersistentCredentialInDotenv(t *testing.T) {
	tests := []struct {
		name string
		file string // "" => no file written
		body string
		want bool
	}{
		{"env_local_with_key", ".env.local", "MISTRAL_API_KEY=sk-real-value\n", true},
		{"env_local_export_prefix", ".env.local", "export OPENAI_API_KEY=abc123\n", true},
		{"env_local_quoted", ".env.local", "MISTRAL_API_KEY=\"quoted-secret\"\n", true},
		{"env_fallback", ".env", "COHERE_API_KEY=xyz\n", true},
		{"empty_value", ".env.local", "MISTRAL_API_KEY=\n", false},
		{"unrelated_key", ".env.local", "SOME_OTHER_VAR=1\n", false},
		{"comment_only", ".env.local", "# MISTRAL_API_KEY=ignored\n", false},
		{"no_file", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0o600); err != nil {
					t.Fatalf("write %s: %v", tc.file, err)
				}
			}
			if got := persistentCredentialInDotenv(dir); got != tc.want {
				t.Errorf("persistentCredentialInDotenv = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPersistCredentialsFromEnv pins the auto-save behaviour: any
// serviceCredentialEnvKeys key found in the current environment is written
// to .env.local so the launchd service can read it after a reboot.
func TestPersistCredentialsFromEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")

	t.Setenv("MISTRAL_API_KEY", "sk-live-value")
	t.Setenv("OPENAI_API_KEY", "")

	saved := persistCredentialsFromEnv(envPath)
	if len(saved) != 1 || saved[0] != "MISTRAL_API_KEY" {
		t.Fatalf("expected [MISTRAL_API_KEY], got %v", saved)
	}

	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env.local: %v", err)
	}
	if !strings.Contains(string(body), "MISTRAL_API_KEY=sk-live-value") {
		t.Errorf(".env.local missing expected key=value:\n%s", body)
	}
	if strings.Contains(string(body), "OPENAI_API_KEY") {
		t.Errorf(".env.local should not contain empty key OPENAI_API_KEY:\n%s", body)
	}

	// File must be written 0o600 (owner-read-write only).
	fi, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat .env.local: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf(".env.local permissions = %v, want 0600", fi.Mode().Perm())
	}
}

// TestPersistCredentialsFromEnv_NoneSet returns empty when no known key
// is in the environment.
func TestPersistCredentialsFromEnv_NoneSet(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	for _, k := range serviceCredentialEnvKeys {
		t.Setenv(k, "")
	}
	saved := persistCredentialsFromEnv(envPath)
	if len(saved) != 0 {
		t.Errorf("expected nothing saved, got %v", saved)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Error(".env.local should not be created when no credentials are set")
	}
}

// TestPersistCredentialsFromEnv_Upsert verifies the existing value in
// .env.local is replaced when a newer value is in the environment.
func TestPersistCredentialsFromEnv_Upsert(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(envPath, []byte("MISTRAL_API_KEY=old-value\n"), 0o600); err != nil {
		t.Fatalf("seed .env.local: %v", err)
	}

	t.Setenv("MISTRAL_API_KEY", "new-value")
	persistCredentialsFromEnv(envPath)

	body, _ := os.ReadFile(envPath)
	if !strings.Contains(string(body), "MISTRAL_API_KEY=new-value") {
		t.Errorf("expected upserted new-value:\n%s", body)
	}
	if strings.Contains(string(body), "old-value") {
		t.Errorf("old-value should have been replaced:\n%s", body)
	}
}

// TestValidateServiceName pins path-traversal rejection for explicit
// --name overrides. Auto-derived names are safe; this guards operator
// overrides that could embed path separators.
func TestValidateServiceName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{"dir2mcp-legal-abc123", false},
		{"com.dirstral.foo", false},
		{"simple", false},
		{"../../etc/evil", true},
		{"/etc/evil", true},
		{"foo/bar", true},
		{`foo\bar`, true},
	} {
		err := validateServiceName(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateServiceName(%q): got err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

// TestRunService_RejectsBadSubcommand pins the dispatch guard rails that
// run before any OS backend is touched, so they behave identically on
// every platform.
func TestRunService_RejectsBadSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		var out, errBuf bytes.Buffer
		app := NewAppWithIO(&out, &errBuf)
		code := app.runService(context.Background(), globalOptions{}, args)
		if code != exitConfigInvalid {
			t.Errorf("args=%v: code=%d, want %d", args, code, exitConfigInvalid)
		}
		if !strings.Contains(errBuf.String(), "subcommand") {
			t.Errorf("args=%v: stderr missing subcommand hint: %q", args, errBuf.String())
		}
	}
}
