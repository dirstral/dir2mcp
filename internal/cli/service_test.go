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

	// KeepAlive must be a dict with SuccessfulExit=false (respawn on crash
	// only, not on a graceful exit-0 stop), not an unconditional <true/>.
	// Guard on the substring order so KeepAlive is followed by the dict.
	keepAliveDict := "<key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>"
	if !strings.Contains(out, keepAliveDict) {
		t.Errorf("plist KeepAlive is not a SuccessfulExit=false dict:\n%s", out)
	}
	if strings.Contains(out, "<key>KeepAlive</key>\n  <true/>") {
		t.Errorf("plist still uses unconditional KeepAlive <true/>:\n%s", out)
	}
	// ThrottleInterval widens the default 10s respawn backoff.
	if !strings.Contains(out, "<key>ThrottleInterval</key>\n  <integer>30</integer>") {
		t.Errorf("plist missing ThrottleInterval=30:\n%s", out)
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

// TestRenderSystemdUnit pins the systemd user-unit contract: the
// crash-loop-safe supervision block (Restart=on-failure + backoff +
// StartLimit), the optional .env.local EnvironmentFile, append: log
// redirection, and the INI-specific escaping (%→%%, space-quoted ExecStart)
// that is the analog of the plist XML escaping. Unit-tested on every platform.
func TestRenderSystemdUnit(t *testing.T) {
	spec := serviceSpec{
		Label:      "com.dirstral.dir2mcp-demo-abc123",
		BinaryPath: "/usr/local/bin/dir2mcp",
		WorkingDir: "/srv/corpus 50%",
		Args:       []string{"up", "--foreground", "--config", "/srv/my config.yaml"},
		LogPath:    "/srv/corpus 50%/.dir2mcp/service.log",
	}
	out := renderSystemdUnit(spec)

	for _, want := range []string{
		"[Unit]",
		"[Service]",
		"Type=simple",
		"[Install]",
		"WantedBy=default.target",
		"Restart=on-failure",
		"RestartSec=30",
		"StartLimitIntervalSec=300",
		"StartLimitBurst=5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("systemd unit missing %q\n---\n%s", want, out)
		}
	}

	// EnvironmentFile points at the corpus .env.local and is optional (leading
	// `-`), with % doubled to %%.
	if !strings.Contains(out, "EnvironmentFile=-/srv/corpus 50%%/.env.local") {
		t.Errorf("systemd unit missing optional .env.local EnvironmentFile with %%%% escape:\n%s", out)
	}

	// Log redirection must use append: so the log survives restarts.
	if !strings.Contains(out, "StandardOutput=append:/srv/corpus 50%%/.dir2mcp/service.log") {
		t.Errorf("systemd unit StandardOutput not append:-redirected/escaped:\n%s", out)
	}
	if !strings.Contains(out, "StandardError=append:/srv/corpus 50%%/.dir2mcp/service.log") {
		t.Errorf("systemd unit StandardError not append:-redirected/escaped:\n%s", out)
	}

	// WorkingDirectory: literal % doubled.
	if !strings.Contains(out, "WorkingDirectory=/srv/corpus 50%%") {
		t.Errorf("systemd unit WorkingDirectory not %%-escaped:\n%s", out)
	}

	// ExecStart: binary + args, with the space-containing token quoted.
	if !strings.Contains(out, `ExecStart=/usr/local/bin/dir2mcp up --foreground --config "/srv/my config.yaml"`) {
		t.Errorf("systemd unit ExecStart not space-quoted:\n%s", out)
	}

	// A raw single-% must never survive (would be a systemd specifier).
	if strings.Contains(strings.ReplaceAll(out, "%%", ""), "%") {
		t.Errorf("systemd unit contains an unescaped single %%:\n%s", out)
	}
}

// TestRejectMultilineSpec pins the newline guard: a value with \r or \n must
// be rejected (it would break the INI key=value line format), the same
// protection persistCredentialsFromEnv applies to dotenv values.
func TestRejectMultilineSpec(t *testing.T) {
	ok := serviceSpec{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", Args: []string{"up"}, LogPath: "/x/l.log"}
	if err := rejectMultilineSpec(ok); err != nil {
		t.Errorf("clean spec rejected: %v", err)
	}
	for _, bad := range []serviceSpec{
		{Label: "com.dirstral.x\n", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x"},
		{Label: "com.dirstral.x", BinaryPath: "/bin/dir2\rmcp", WorkingDir: "/x"},
		{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", Args: []string{"up\n--foreground"}},
		{Label: "com.dirstral.x", BinaryPath: "/bin/dir2mcp", WorkingDir: "/x", LogPath: "/x/l\n.log"},
	} {
		if err := rejectMultilineSpec(bad); err == nil {
			t.Errorf("expected rejection for spec with newline: %+v", bad)
		}
	}
}

// TestResolveProcessExitCode pins the #434 exit-code contract: a
// signal-triggered graceful stop of the long-running server exits 0 (so
// launchd/systemd don't treat a `down` as a crash and respawn), while an
// interactive command interrupted mid-flight still reports the interrupt code
// and any crash keeps its own non-zero code.
func TestResolveProcessExitCode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ctxErr      error
		code        int
		gracefulSrv bool
		want        int
	}{
		{"server graceful SIGTERM exits 0", context.Canceled, exitSuccess, true, exitSuccess},
		{"interactive interrupt maps to signal code", context.Canceled, exitSuccess, false, exitSignalInterrupt},
		{"server crash keeps its non-zero code", context.Canceled, exitGeneric, true, exitGeneric},
		{"clean exit without cancel unchanged", nil, exitSuccess, false, exitSuccess},
		{"deadline (not cancel) unchanged", context.DeadlineExceeded, exitSuccess, false, exitSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProcessExitCode(tc.ctxErr, tc.code, tc.gracefulSrv); got != tc.want {
				t.Errorf("resolveProcessExitCode(%v,%d,%v)=%d, want %d", tc.ctxErr, tc.code, tc.gracefulSrv, got, tc.want)
			}
		})
	}
}

func TestPersistentCredentialInDotenv(t *testing.T) {
	refs := []string{
		"MISTRAL_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
		"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "COHERE_API_KEY", "ELEVENLABS_API_KEY",
	}
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
			if got := persistentCredentialInDotenv(dir, refs); got != tc.want {
				t.Errorf("persistentCredentialInDotenv = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPersistCredentialsFromEnv pins the auto-save behaviour: any key in
// envVarRefs found in the current environment is written to .env.local so
// the launchd service can read it after a reboot.
func TestPersistCredentialsFromEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")

	t.Setenv("MISTRAL_API_KEY", "sk-live-value")
	t.Setenv("OPENAI_API_KEY", "")

	saved, _ := persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY", "OPENAI_API_KEY"})
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

// TestPersistCredentialsFromEnv_NoneSet returns empty when no key in
// envVarRefs is in the environment.
func TestPersistCredentialsFromEnv_NoneSet(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	saved, _ := persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY", "OPENAI_API_KEY"})
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
	persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY"})

	body, _ := os.ReadFile(envPath)
	if !strings.Contains(string(body), "MISTRAL_API_KEY=new-value") {
		t.Errorf("expected upserted new-value:\n%s", body)
	}
	if strings.Contains(string(body), "old-value") {
		t.Errorf("old-value should have been replaced:\n%s", body)
	}
}

// TestPersistCredentialsFromEnv_ExportPrefixUpsert verifies that an existing
// "export KEY=old" line in .env.local is replaced — not left alongside — when
// the new value is written, so the earlier export line cannot shadow the update.
func TestPersistCredentialsFromEnv_ExportPrefixUpsert(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(envPath, []byte("export MISTRAL_API_KEY=old-value\n"), 0o600); err != nil {
		t.Fatalf("seed .env.local: %v", err)
	}

	t.Setenv("MISTRAL_API_KEY", "new-value")
	persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY"})

	body, _ := os.ReadFile(envPath)
	if !strings.Contains(string(body), "MISTRAL_API_KEY=new-value") {
		t.Errorf("expected new-value to be written:\n%s", body)
	}
	if strings.Contains(string(body), "old-value") {
		t.Errorf("old-value from export line should have been removed:\n%s", body)
	}
}

// TestPersistCredentialsFromEnv_CustomKey pins that envVarRefs is not
// limited to built-in providers: a custom key like MY_PROVIDER_API_KEY
// supplied via providers: api_key: "${MY_PROVIDER_API_KEY}" in YAML is
// also saved when found in the current environment.
func TestPersistCredentialsFromEnv_CustomKey(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	t.Setenv("MY_PROVIDER_API_KEY", "custom-secret")
	saved, _ := persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY", "MY_PROVIDER_API_KEY"})
	if len(saved) != 1 || saved[0] != "MY_PROVIDER_API_KEY" {
		t.Fatalf("expected [MY_PROVIDER_API_KEY], got %v", saved)
	}
	body, _ := os.ReadFile(envPath)
	if !strings.Contains(string(body), "MY_PROVIDER_API_KEY=custom-secret") {
		t.Errorf(".env.local missing custom key:\n%s", body)
	}
}

// TestPersistCredentialsFromEnv_NewlineRejected pins that values containing
// \r or \n are placed in the failed list rather than written to .env.local,
// preventing newline injection that could corrupt the file format.
func TestPersistCredentialsFromEnv_NewlineRejected(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	t.Setenv("MISTRAL_API_KEY", "good-value")
	t.Setenv("OPENAI_API_KEY", "bad\nvalue")

	saved, failed := persistCredentialsFromEnv(envPath, []string{"MISTRAL_API_KEY", "OPENAI_API_KEY"})
	if len(saved) != 1 || saved[0] != "MISTRAL_API_KEY" {
		t.Errorf("saved = %v, want [MISTRAL_API_KEY]", saved)
	}
	if len(failed) != 1 || failed[0] != "OPENAI_API_KEY" {
		t.Errorf("failed = %v, want [OPENAI_API_KEY]", failed)
	}
	body, _ := os.ReadFile(envPath)
	if strings.Contains(string(body), "OPENAI_API_KEY") {
		t.Errorf(".env.local must not contain rejected key: %s", body)
	}
}

// TestValidateServiceName pins the allowlist validation for explicit --name
// overrides. Only A-Za-z0-9._- are permitted; whitespace, control chars,
// path separators, and absolute paths must all be rejected.
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
		{"foo bar", true},        // space not allowed
		{" ", true},              // whitespace-only
		{"hello\x00world", true}, // null byte
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
