package tests

import (
	"bytes"
	"strings"
	"testing"

	"dir2mcp/internal/cli"
)

func TestDir2MCPVersionCommand_UsesBuildVersion(t *testing.T) {
	oldVersion := cli.Version
	cli.Version = "v9.9.9-test"
	t.Cleanup(func() { cli.Version = oldVersion })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.Run([]string{"version"})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%q", code, stderr.String())
	}

	got := strings.TrimSpace(stdout.String())
	if got != "dir2mcp v9.9.9-test" {
		t.Fatalf("unexpected version output: %q", got)
	}
}
