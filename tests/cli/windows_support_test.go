package tests

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// The Claude Desktop config lives in %APPDATA%\Claude on Windows. The helper
// is pure, so this test checks every platform branch on every OS.
func TestClaudeDesktopConfigPath_PerPlatform(t *testing.T) {
	const name = "claude_desktop_config.json"
	home := filepath.Join("home", "user")
	appData := filepath.Join("C", "Users", "user", "AppData", "Roaming")
	cases := []struct {
		label, goos, home, appData, want string
	}{
		{"windows uses APPDATA", "windows", home, appData, filepath.Join(appData, "Claude", name)},
		{"windows without APPDATA uses the roaming profile", "windows", home, "", filepath.Join(home, "AppData", "Roaming", "Claude", name)},
		{"windows without any base dir", "windows", "", "  ", name},
		{"darwin is unchanged", "darwin", home, appData, filepath.Join(home, "Library", "Application Support", "Claude", name)},
		{"linux is unchanged", "linux", home, "", filepath.Join(home, "Library", "Application Support", "Claude", name)},
		{"no home dir", "darwin", "", "", name},
	}
	for _, tc := range cases {
		if got := cli.ClaudeDesktopConfigPathForTest(tc.goos, tc.home, tc.appData); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.label, got, tc.want)
		}
	}
}

// `dir2mcp service` has no Windows backend. The error must say so and name the
// manual alternative, instead of a bare "not supported".
func TestServiceUnsupportedMessage_WindowsNamesAlternative(t *testing.T) {
	msg := cli.ServiceUnsupportedMessageForTest("windows")
	for _, want := range []string{"not supported on windows", "dir2mcp up --foreground", "Task Scheduler"} {
		if !strings.Contains(msg, want) {
			t.Errorf("windows service message lacks %q: %s", want, msg)
		}
	}
	other := cli.ServiceUnsupportedMessageForTest("freebsd")
	if strings.Contains(other, "Task Scheduler") {
		t.Errorf("non-windows message must not name Task Scheduler: %s", other)
	}
}

// On a platform without daemon mode (Windows), `up --daemon` must fail with a
// clear error instead of a silent foreground run. On unix the flag forks a
// real daemon child, which a unit test must not do, so the test runs on
// Windows only.
func TestUpDaemonFlag_RejectedOnWindows(t *testing.T) {
	if !isWindows() {
		t.Skip("unix supports --daemon; the flag would fork a real daemon child")
	}
	t.Setenv("MISTRAL_API_KEY", "")
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	var code int
	withWorkingDir(t, t.TempDir(), func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		code = app.RunWithContext(ctx, []string{"up", "--daemon", "--listen", "127.0.0.1:0"})
	})
	if code == 0 || !strings.Contains(stderr.String(), "--daemon is not supported on windows") {
		t.Fatalf("up --daemon must fail with a clear error; code=%d stderr=%s", code, stderr.String())
	}
}

// posixModesUnsupported reports whether the platform lacks POSIX mode bits.
// Windows reports 0666 or 0444 for every file, so owner-only (0600) checks
// cannot hold there.
func posixModesUnsupported() bool { return isWindows() }

// skipOnWindows skips a test that needs a unix-only feature. The reason
// names the feature, so a skipped test is never silent.
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if isWindows() {
		t.Skip(reason)
	}
}
