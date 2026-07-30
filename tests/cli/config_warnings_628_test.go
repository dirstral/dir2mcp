package tests

// #628 part 2: Config.Warnings was appended to but never read by any production
// code path, so non-fatal config diagnostics — a deprecated env var, an
// unparseable duration, and now an unrecognized config key — were collected and
// then silently discarded. `up` now prints them to stderr before the startup
// preflights, so an operator whose config was partly ignored is told.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

func writeConfigWithUnknownKeys(t *testing.T, dir string) {
	t.Helper()
	body := "" +
		"root_dir: .\n" +
		"stt_provider: off\n" +
		"recognise_provider: serve\n" + // misspelling
		"some_stale_key: 1\n" // stale key from an older release
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// The warning must reach stderr even though `up` later fails its embed preflight:
// the point is that config problems surface, not that startup succeeds.
func TestUp_WarnsAboutUnrecognizedConfigKeys(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	writeConfigWithUnknownKeys(t, tmp)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	withWorkingDir(t, tmp, func() {
		_ = app.RunWithContext(context.Background(),
			[]string{"up", "--non-interactive", "--foreground", "--listen", "127.0.0.1:0"})
	})

	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Fatalf("stderr carries no warning; config problems are still silent.\nstderr: %s", got)
	}
	for _, want := range []string{"unrecognized", "recognise_provider", "some_stale_key"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nstderr: %s", want, got)
		}
	}
}

// --quiet suppresses non-error output, warnings included.
func TestUp_QuietSuppressesConfigWarnings(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	writeConfigWithUnknownKeys(t, tmp)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	withWorkingDir(t, tmp, func() {
		_ = app.RunWithContext(context.Background(),
			[]string{"up", "--quiet", "--non-interactive", "--foreground", "--listen", "127.0.0.1:0"})
	})

	if strings.Contains(stderr.String(), "unrecognized") {
		t.Errorf("--quiet must suppress config warnings; stderr: %s", stderr.String())
	}
}

// A valid config must stay silent — a false-positive warning on good config
// would be worse than the silence this replaces.
func TestUp_ValidConfigEmitsNoUnrecognizedKeyWarning(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	body := "root_dir: .\nstt_provider: off\nrag_k_default: 7\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	withWorkingDir(t, tmp, func() {
		_ = app.RunWithContext(context.Background(),
			[]string{"up", "--non-interactive", "--foreground", "--listen", "127.0.0.1:0"})
	})

	if strings.Contains(stderr.String(), "unrecognized") {
		t.Errorf("valid config must not warn; stderr: %s", stderr.String())
	}
}
