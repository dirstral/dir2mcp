package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestHelpFlag_PrintsUsageAndSucceeds pins that -h/-help/--help work on the
// bare binary and on every command shape: the usage goes to stdout, stderr
// stays empty and the exit code is 0. Before the fix each command's flag set
// reported flag.ErrHelp as invalid flags and exited 2, and the bare binary
// rejected --help as an unknown global flag.
func TestHelpFlag_PrintsUsageAndSucceeds(t *testing.T) {
	cases := [][]string{
		{"--help"},
		{"-h"},
		{"--json", "--help"},
		{"up", "--help"},
		{"up", "--listen", "127.0.0.1:0", "-h"},
		{"status", "--help"},
		{"config", "--help"},
		{"config", "init", "--help"},
		{"install", "claude", "--help"},
		{"ask", "-help"},
		{"reindex", "--help"},
		{"service", "install", "--help"},
		{"version", "--help"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Chdir(t.TempDir())
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)
			code := app.RunWithContext(context.Background(), args)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{"Usage", "Commands", "dir2mcp [global flags] <command>"} {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout does not contain %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestHelpFlag_AfterTerminatorIsAnOperand pins that a help token after "--"
// is an operand and not a help request. version rejects any operand, so the
// command must fail and must not print the usage.
func TestHelpFlag_AfterTerminatorIsAnOperand(t *testing.T) {
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{"version", "--", "--help"})
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero; stdout=%q", stdout.String())
	}
	if strings.Contains(stdout.String(), "Commands") {
		t.Fatalf("usage printed for an operand after --:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not accept arguments") {
		t.Fatalf("stderr = %q, want the version operand rejection", stderr.String())
	}
}
