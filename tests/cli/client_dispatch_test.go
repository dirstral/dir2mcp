package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// The action-first client verbs (install/uninstall/doctor/print-config)
// share one dispatcher. These tests pin the two user-facing error
// paths that dispatcher owns — missing <client> positional and an
// unsupported <client> — across every verb, in both human and JSON
// output modes. Without this, a regression in those branches would go
// uncaught since the happy-path tests only exercise `claude`.

var clientVerbs = []string{"install", "uninstall", "doctor", "print-config"}

func TestClientVerb_MissingClient_Human(t *testing.T) {
	for _, verb := range clientVerbs {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)

			code := app.RunWithContext(context.Background(), []string{verb})

			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (CONFIG_INVALID); stderr=%s", code, stderr.String())
			}
			errOut := stderr.String()
			if !strings.Contains(errOut, "requires a client name") {
				t.Fatalf("stderr missing 'requires a client name': %q", errOut)
			}
			if !strings.Contains(errOut, "claude") {
				t.Fatalf("stderr should list supported clients incl. claude: %q", errOut)
			}
			if !strings.Contains(errOut, "dir2mcp "+verb+" claude") {
				t.Fatalf("stderr should show the example invocation: %q", errOut)
			}
		})
	}
}

func TestClientVerb_MissingClient_JSON(t *testing.T) {
	for _, verb := range clientVerbs {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)

			code := app.RunWithContext(context.Background(), []string{"--json", verb})

			if code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
			}
			payload := decodeCLIError(t, stderr.Bytes())
			if payload.ExitCode != 2 {
				t.Fatalf("payload exit_code = %d, want 2", payload.ExitCode)
			}
			if payload.Error.Code != "CONFIG_INVALID" {
				t.Fatalf("payload error.code = %q, want CONFIG_INVALID", payload.Error.Code)
			}
			if !strings.Contains(payload.Error.Message, "requires a client name") {
				t.Fatalf("payload error.message unexpected: %q", payload.Error.Message)
			}
		})
	}
}

func TestClientVerb_UnknownClient_Human(t *testing.T) {
	for _, verb := range clientVerbs {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)

			code := app.RunWithContext(context.Background(), []string{verb, "chatgpt"})

			if code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
			}
			errOut := stderr.String()
			if !strings.Contains(errOut, "unknown client") || !strings.Contains(errOut, "chatgpt") {
				t.Fatalf("stderr should name the unknown client: %q", errOut)
			}
			if !strings.Contains(errOut, "claude") {
				t.Fatalf("stderr should list supported clients incl. claude: %q", errOut)
			}
		})
	}
}

func TestClientVerb_UnknownClient_JSON(t *testing.T) {
	for _, verb := range clientVerbs {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)

			code := app.RunWithContext(context.Background(), []string{"--json", verb, "chatgpt"})

			if code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
			}
			payload := decodeCLIError(t, stderr.Bytes())
			if payload.Error.Code != "CONFIG_INVALID" {
				t.Fatalf("payload error.code = %q, want CONFIG_INVALID", payload.Error.Code)
			}
			if !strings.Contains(payload.Error.Message, "unknown client") {
				t.Fatalf("payload error.message unexpected: %q", payload.Error.Message)
			}
		})
	}
}

// TestClientVerb_ClientNameCaseInsensitive confirms the dispatcher
// lowercases the positional, so `Claude` routes the same as `claude`
// (it reaches the claude leaf and fails later on missing connection
// state, not at the dispatcher with an unknown-client error).
func TestClientVerb_ClientNameCaseInsensitive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{"--state-dir", t.TempDir(), "doctor", "CLAUDE"})

	if code == 2 && strings.Contains(stderr.String(), "unknown client") {
		t.Fatalf("`CLAUDE` should route to the claude leaf, not unknown-client: %q", stderr.String())
	}
}

type cliErrorEnvelope struct {
	Error struct {
		Code    string   `json:"code"`
		Message string   `json:"message"`
		Hints   []string `json:"hints"`
	} `json:"error"`
	ExitCode int `json:"exit_code"`
}

func decodeCLIError(t *testing.T, raw []byte) cliErrorEnvelope {
	t.Helper()
	var env cliErrorEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		t.Fatalf("decode CLI error JSON: %v raw=%s", err, string(raw))
	}
	return env
}
