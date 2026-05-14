package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

func TestUpModelFlags_IsAcceptedByCLI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{
			"--non-interactive",
			"up",
			"--embed-model-text", "foo",
			"--embed-model-code", "bar",
			"--chat-model", "baz",
		})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	errText := stderr.String()
	if strings.Contains(errText, "invalid up flags") {
		t.Fatalf("flag should be accepted by parser, got: %s", errText)
	}
	if !strings.Contains(errText, "MISTRAL_API_KEY") {
		t.Fatalf("expected config validation failure after parsing flags, got: %s", errText)
	}
}
