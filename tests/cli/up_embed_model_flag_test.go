package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// The legacy model-override flags (--embed-model-text/-code,
// --chat-model, --mistral-max-ocr-payload-bytes) were removed in the
// provider clean break (#38): model selection is configured via
// providers:/model: in .dir2mcp.yaml. The parser must now reject them
// so the removal is explicit and guarded against re-introduction.
func TestUpModelFlags_RemovedInCleanBreak(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")

	cases := [][]string{
		{"--embed-model-text", "foo"},
		{"--embed-model-code", "bar"},
		{"--chat-model", "baz"},
		{"--mistral-max-ocr-payload-bytes", "1"},
	}
	for _, flag := range cases {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		withWorkingDir(t, tmp, func() {
			args := append([]string{"--non-interactive", "up"}, flag...)
			code := app.RunWithContext(context.Background(), args)
			if code != 2 {
				t.Fatalf("%s: expected exit 2, got %d stderr=%s", flag[0], code, stderr.String())
			}
		})
		if !strings.Contains(stderr.String(), "invalid up flags") {
			t.Fatalf("%s: expected 'invalid up flags' rejection, got: %s", flag[0], stderr.String())
		}
	}
}
