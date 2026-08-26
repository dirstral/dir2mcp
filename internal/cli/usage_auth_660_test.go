package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestUsage_AuthFlagDocumentsModeNotToken pins the fix for #660: the
// top-level usage screen documented --auth as a literal bearer token, while
// the up parser and runtime interpret the value as an auth mode
// (auto | none | file:<path>). The usage screen and the up flag parser now
// share authFlagUsage, and this test asserts the rendered help text carries
// that exact string so the two surfaces cannot drift apart again.
func TestUsage_AuthFlagDocumentsModeNotToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), nil)
	if code != exitSuccess {
		t.Fatalf("usage exit code = %d, want %d (stderr: %s)", code, exitSuccess, stderr.String())
	}
	usage := stdout.String()

	if strings.Contains(usage, "--auth <token>") {
		t.Fatal("usage still documents --auth as a literal token (--auth <token>)")
	}
	if !strings.Contains(usage, "--auth <mode>") {
		t.Fatal("usage does not document --auth as a mode (--auth <mode>)")
	}
	if !strings.Contains(usage, authFlagUsage) {
		t.Fatalf("usage does not carry the flag parser description %q; the two surfaces drifted", authFlagUsage)
	}

	// The shared description must name every supported mode and must point
	// literal-token users at the env var instead of the flag.
	for _, want := range []string{"auto", "none", "file:<path>", authTokenEnvVar} {
		if !strings.Contains(authFlagUsage, want) {
			t.Errorf("authFlagUsage %q does not mention %q", authFlagUsage, want)
		}
	}
}

// TestPrepareAuthMaterial_RejectsLiteralToken pins the runtime contract the
// usage text now describes: the --auth value is a mode, and a literal bearer
// token passed as the value is rejected as an unsupported mode.
func TestPrepareAuthMaterial_RejectsLiteralToken(t *testing.T) {
	var warn bytes.Buffer

	_, err := prepareAuthMaterial(config.Config{AuthMode: "s3cr3t-literal-token"}, &warn)
	if err == nil {
		t.Fatal("prepareAuthMaterial accepted a literal token as an auth mode")
	}
	if !strings.Contains(err.Error(), "unsupported auth mode") {
		t.Fatalf("error = %q, want it to name the unsupported auth mode", err)
	}

	auth, err := prepareAuthMaterial(config.Config{AuthMode: "none"}, &warn)
	if err != nil {
		t.Fatalf("prepareAuthMaterial(none): %v", err)
	}
	if auth.mode != "none" {
		t.Fatalf("auth mode = %q, want %q", auth.mode, "none")
	}
}
