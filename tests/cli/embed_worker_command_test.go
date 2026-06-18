package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestEmbedWorkerAppearsInUsage verifies the subcommand is advertised in the
// top-level usage block (dir2mcp has no `help` subcommand; usage prints on
// no-arg invocation).
func TestEmbedWorkerAppearsInUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), nil)
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "embed-worker") {
		t.Fatalf("expected embed-worker in usage, got:\n%s", stdout.String())
	}
}

// TestEmbedWorkerIsRegistered confirms the command is dispatched (not reported
// as an unknown command). Running it in an empty dir with no distributed config
// must fail with a remediable CONFIG_INVALID (exit 2), never the unknown-command
// path (exit 1).
func TestEmbedWorkerIsRegistered(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"embed-worker"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})
	if strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("embed-worker should be a registered command, got: %s", stderr.String())
	}
}

// TestEmbedWorkerFailsFastWhenDistributedDisabled verifies the worker refuses to
// run when distributed embedding is not enabled — it has no role otherwise.
func TestEmbedWorkerFailsFastWhenDistributedDisabled(t *testing.T) {
	tmp := t.TempDir()
	// A plain config with distributed mode off (the default).
	writeWorkerConfig(t, tmp, "root_dir: .\nstate_dir: .dir2mcp\n")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "embed-worker"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	payload := decodeCLIError(t, stderr.Bytes())
	if payload.Error.Code != "CONFIG_INVALID" || payload.ExitCode != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.Contains(payload.Error.Message, "requires distributed embedding to be enabled") {
		t.Fatalf("unexpected message: %q", payload.Error.Message)
	}
}

// TestEmbedWorkerFailsFastWithoutTierCStore verifies that enabling distributed
// embedding over a single-node embedded backend (memory) is rejected: a worker
// pool requires a shared Tier-C store (SPEC §8.7.4). The rejection is a
// remediable CONFIG_INVALID (exit 2) regardless of which validation layer
// catches it.
func TestEmbedWorkerFailsFastWithoutTierCStore(t *testing.T) {
	tmp := t.TempDir()
	writeWorkerConfig(t, tmp, "root_dir: .\nstate_dir: .dir2mcp\nindex_backend: memory\ndistributed_embed_enabled: true\n")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "embed-worker"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	payload := decodeCLIError(t, stderr.Bytes())
	if payload.Error.Code != "CONFIG_INVALID" || payload.ExitCode != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.Contains(strings.ToLower(payload.Error.Message), "tier c") &&
		!strings.Contains(strings.ToLower(payload.Error.Message), "tier-c") {
		t.Fatalf("expected a Tier-C prerequisite error, got: %q", payload.Error.Message)
	}
}

// TestEmbedWorkerRejectsUnexpectedArgs verifies flag parsing: the subcommand
// takes no positional arguments.
func TestEmbedWorkerRejectsUnexpectedArgs(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "embed-worker", "extra-arg"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	payload := decodeCLIError(t, stderr.Bytes())
	if payload.Error.Code != "CONFIG_INVALID" || payload.ExitCode != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.Contains(payload.Error.Message, "invalid embed-worker flags") {
		t.Fatalf("unexpected message: %q", payload.Error.Message)
	}
}

// TestEmbedWorkerRejectsUnknownFlag verifies an unknown flag is a parse error.
func TestEmbedWorkerRejectsUnknownFlag(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"embed-worker", "--nope"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "invalid embed-worker flags") {
		t.Fatalf("expected flag parse error, got: %s", stderr.String())
	}
}

// TestEmbedWorkerRejectsBadDurationFlag verifies a malformed duration knob is a
// parse error rather than being silently ignored.
func TestEmbedWorkerRejectsBadDurationFlag(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"embed-worker", "--lease-duration", "not-a-duration"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "invalid embed-worker flags") {
		t.Fatalf("expected flag parse error, got: %s", stderr.String())
	}
}

// writeWorkerConfig writes a .dir2mcp.yaml into dir.
func writeWorkerConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
