//go:build unix

package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"dir2mcp/internal/cli"
)

// TestDaemonParent_MissingMistralAPIKeyFailsFast is the regression test
// for issue #178. Before the fix, `dir2mcp up --daemon` with no
// MISTRAL_API_KEY would fork a child that exited instantly on
// CONFIG_INVALID, and the parent would surface a misleading 15s
// "did not become ready" timeout instead of the real config error.
//
// The fix moves the cfg-only preconditions (--public requires auth,
// missing MISTRAL_API_KEY) into the daemon parent BEFORE spawn, so a
// misconfig produces the same instant CONFIG_INVALID error the
// in-process body would have emitted.
//
// We assert two things:
//
//  1. The call returns the expected CONFIG_INVALID exit code with the
//     "Missing MISTRAL_API_KEY" message on stderr.
//  2. It returns quickly — well under the 15s readiness timeout the
//     buggy fork-then-wait path would have hit. A 5s budget is plenty
//     of headroom for a slow CI runner while still being a fraction of
//     the 15s window we're regression-guarding against.
//
// Forks never happen because the preflight fails first, so this test
// stays in the unit budget (no RUN_INTEGRATION_TESTS gate).
func TestDaemonParent_MissingMistralAPIKeyFailsFast(t *testing.T) {
	tmp := t.TempDir()
	// Explicitly clear MISTRAL_API_KEY for this test even if the host
	// environment has it set — the whole point is the missing-key path.
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	start := time.Now()
	var code int
	withWorkingDir(t, tmp, func() {
		// --daemon forces shouldDaemonize() to return true even though
		// stdout is a *bytes.Buffer (not a TTY). That's the route the
		// parent would take in a real interactive shell.
		// --listen 127.0.0.1:0 isn't strictly necessary (the preflight
		// fails before bind), but keeps the test deterministic if the
		// preflight order ever changes.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"up", "--daemon", "--listen", "127.0.0.1:0"})
	})
	elapsed := time.Since(start)

	const exitConfigInvalid = 2
	if code != exitConfigInvalid {
		t.Fatalf("exit code: got=%d want=%d stderr=%q", code, exitConfigInvalid, stderr.String())
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "MISTRAL_API_KEY") {
		t.Errorf("stderr should mention MISTRAL_API_KEY, got: %q", stderrStr)
	}
	if strings.Contains(stderrStr, "did not become ready") {
		t.Errorf("stderr should NOT contain the 15s readiness timeout (issue #178); got: %q", stderrStr)
	}

	// 5s is generous headroom over the cfg-only preflight (microseconds
	// of work) while staying well clear of the 15s readiness timeout
	// the pre-fix code would have hit.
	if elapsed > 5*time.Second {
		t.Errorf("preflight should fail fast (well under 15s readiness timeout); elapsed=%s", elapsed)
	}
}
