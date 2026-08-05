package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
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

	// Start-rate limits are [Unit]-level directives; systemd ignores them under
	// [Service]. Assert they appear before the [Service] header so the crash-loop
	// cap actually applies.
	if svc := strings.Index(out, "[Service]"); svc >= 0 {
		if lim := strings.Index(out, "StartLimitIntervalSec="); lim < 0 || lim > svc {
			t.Errorf("StartLimitIntervalSec must be in [Unit] (before [Service]):\n%s", out)
		}
		if burst := strings.Index(out, "StartLimitBurst="); burst < 0 || burst > svc {
			t.Errorf("StartLimitBurst must be in [Unit] (before [Service]):\n%s", out)
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

// TestIniEscapeBackslash pins that a value ending in `\` is doubled so systemd
// does not read it as a line-continuation into the next directive.
func TestIniEscapeBackslash(t *testing.T) {
	if got := iniEscape(`/srv/corpus\`); got != `/srv/corpus\\` {
		t.Errorf("trailing backslash not doubled: %q", got)
	}
	if got := iniEscape(`a\b%c`); got != `a\\b%%c` {
		t.Errorf("backslash+percent escaping wrong: %q", got)
	}
}

// TestRenderSystemdExecStart_EmbeddedQuote pins that a token containing a double
// quote is quoted AND its embedded quote escaped, so it can't break out of the
// quoting and split the command line.
func TestRenderSystemdExecStart_EmbeddedQuote(t *testing.T) {
	spec := serviceSpec{
		BinaryPath: "/usr/local/bin/dir2mcp",
		Args:       []string{"up", `--config`, `/srv/a"b/c.yaml`},
	}
	got := renderSystemdExecStart(spec)
	if !strings.Contains(got, `"/srv/a\"b/c.yaml"`) {
		t.Errorf("embedded quote not escaped+quoted: %q", got)
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

// --- issue #738: remote corpora must not anchor the service on a local root ---

// s3ServiceConfig builds a native-S3 corpus config whose corpus root is a local
// path that does not exist. Per SPEC §7.8 the bucket + prefix IS the corpus root
// for source.kind=s3, so this is a legitimate remote-only deployment.
func s3ServiceConfig(rootDir, stateDir string) config.Config {
	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "my-corpus-bucket"
	cfg.Source.S3Prefix = "corpus/"
	return cfg
}

// For an S3 corpus the supervisor working directory must be a directory that
// actually exists — the config file's own directory — not the ignored local
// corpus root. Before the fix this rendered abs(RootDir), installing a unit
// whose WorkingDirectory does not exist.
func TestServiceContext_S3AnchorsWorkingDirOnConfigDir_738(t *testing.T) {
	tmp := t.TempDir()
	confDir := filepath.Join(tmp, "conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	cfg := s3ServiceConfig(filepath.Join(tmp, "does-not-exist"), filepath.Join(tmp, "state"))

	sc, err := serviceContextFromConfig(cfg, "", filepath.Join(confDir, ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("serviceContextFromConfig: %v", err)
	}
	if sc.workingDir != confDir {
		t.Fatalf("workingDir = %q, want the config directory %q", sc.workingDir, confDir)
	}
	if !isExistingDir(sc.workingDir) {
		t.Fatalf("rendered WorkingDirectory %q does not exist", sc.workingDir)
	}
	if sc.stateDir != filepath.Join(tmp, "state") {
		t.Errorf("stateDir = %q, want the configured absolute state dir", sc.stateDir)
	}
}

// A relative state_dir must resolve against the SAME base the booted daemon
// resolves it against (its working directory), or install would create and log
// into a different directory than the daemon uses.
func TestServiceContext_S3RelativeStateDirFollowsWorkingDir_738(t *testing.T) {
	tmp := t.TempDir()
	cfg := s3ServiceConfig(filepath.Join(tmp, "does-not-exist"), ".dir2mcp")

	sc, err := serviceContextFromConfig(cfg, "", filepath.Join(tmp, ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("serviceContextFromConfig: %v", err)
	}
	if want := filepath.Join(tmp, ".dir2mcp"); sc.stateDir != want {
		t.Fatalf("stateDir = %q, want %q (resolved against the working dir)", sc.stateDir, want)
	}
	if sc.logPath != filepath.Join(tmp, ".dir2mcp", "service.log") {
		t.Errorf("logPath = %q, want it under the resolved state dir", sc.logPath)
	}
}

// When an explicit --config names a path under a directory that does not exist,
// the working directory falls back to the state directory, which is always local
// (SPEC §7.8) and is created before the unit is written.
func TestServiceContext_S3FallsBackToStateDirWhenConfigDirMissing_738(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	cfg := s3ServiceConfig(filepath.Join(tmp, "does-not-exist"), stateDir)

	sc, err := serviceContextFromConfig(cfg, "", filepath.Join(tmp, "gone", "conf.yaml"))
	if err != nil {
		t.Fatalf("serviceContextFromConfig: %v", err)
	}
	if sc.workingDir != stateDir {
		t.Fatalf("workingDir = %q, want the state dir %q", sc.workingDir, stateDir)
	}
}

// The local/NFS contract is byte-identical to the pre-#738 behavior: the working
// directory is the absolute corpus root and a relative state_dir hangs off it,
// regardless of where the config file lives.
func TestServiceContext_LocalAndNFSKeepCorpusRootWorkingDir_738(t *testing.T) {
	for _, kind := range []string{"", "local", "nfs"} {
		tmp := t.TempDir()
		root := filepath.Join(tmp, "corpus")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir corpus: %v", err)
		}
		cfg := config.Default()
		cfg.RootDir = root
		cfg.StateDir = ".dir2mcp"
		cfg.Source.Kind = kind

		sc, err := serviceContextFromConfig(cfg, "", filepath.Join(tmp, "elsewhere", ".dir2mcp.yaml"))
		if err != nil {
			t.Fatalf("kind=%q: serviceContextFromConfig: %v", kind, err)
		}
		if sc.workingDir != root {
			t.Errorf("kind=%q: workingDir = %q, want the corpus root %q", kind, sc.workingDir, root)
		}
		if want := filepath.Join(root, ".dir2mcp"); sc.stateDir != want {
			t.Errorf("kind=%q: stateDir = %q, want %q", kind, sc.stateDir, want)
		}
	}
}

// The rendered unit is what launchd/systemd actually consume, so pin the
// end-to-end result too: an S3 corpus with no local root must render a
// WorkingDirectory that exists on both backends.
func TestRenderedUnitsUseAnExistingWorkingDirForS3_738(t *testing.T) {
	tmp := t.TempDir()
	missingRoot := filepath.Join(tmp, "does-not-exist")
	cfg := s3ServiceConfig(missingRoot, filepath.Join(tmp, "state"))
	sc, err := serviceContextFromConfig(cfg, "", filepath.Join(tmp, ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("serviceContextFromConfig: %v", err)
	}
	spec := serviceSpec{
		Label:      sc.label,
		BinaryPath: sc.binaryPath,
		WorkingDir: sc.workingDir,
		Args:       []string{"up", "--foreground"},
		LogPath:    sc.logPath,
	}
	if !isExistingDir(spec.WorkingDir) {
		t.Fatalf("rendered WorkingDirectory %q does not exist", spec.WorkingDir)
	}
	for name, rendered := range map[string]string{
		"launchd": renderLaunchdPlist(spec),
		"systemd": renderSystemdUnit(spec),
	} {
		if !strings.Contains(rendered, "WorkingDirectory") {
			t.Fatalf("%s unit has no WorkingDirectory:\n%s", name, rendered)
		}
		// The unit must not reference the ignored local corpus root anywhere:
		// WorkingDirectory, the systemd EnvironmentFile, or the log paths.
		if strings.Contains(rendered, missingRoot) {
			t.Errorf("%s unit still points at the ignored local corpus root %q:\n%s", name, missingRoot, rendered)
		}
	}
}

// A relative --config was copied verbatim into the unit and then re-resolved by
// the daemon against ITS working directory, which is not the directory the
// operator ran install from. Both propagated paths are now absolute.
func TestInstallServiceArgs_PropagatesAbsolutePaths_738(t *testing.T) {
	sc := serviceContext{stateDir: filepath.Join("/srv", "corpus", "state")}
	args := installServiceArgs(globalOptions{configPath: "conf/d.yaml", stateDir: "./state"}, sc)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := []string{"up", "--foreground",
		"--config", filepath.Join(cwd, "conf", "d.yaml"),
		"--state-dir", sc.stateDir}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("installServiceArgs = %v, want %v", args, want)
	}

	// No overrides -> no propagated paths (unchanged).
	if got := installServiceArgs(globalOptions{}, sc); strings.Join(got, " ") != "up --foreground" {
		t.Fatalf("installServiceArgs without overrides = %v", got)
	}
}
