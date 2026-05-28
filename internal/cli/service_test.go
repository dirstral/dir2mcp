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
