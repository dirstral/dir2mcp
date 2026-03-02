package tests

import (
	"bytes"
	"strings"
	"testing"

	"dir2mcp/internal/buildinfo"
	"dir2mcp/internal/cli"
)

func TestDir2MCPVersionCommand_UsesBuildVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.Run([]string{"version"})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%q", code, stderr.String())
	}

	got := strings.TrimSpace(stdout.String())
	want := "dir2mcp v" + strings.TrimPrefix(buildinfo.Version, "v")
	if got != want {
		t.Fatalf("unexpected version output: %q", got)
	}
}
