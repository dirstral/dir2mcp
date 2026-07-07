package tests

import (
	"os"
	"testing"
)

// TestMain keeps this package's `up` command tests hermetic by disabling the
// §2.5 embedding-credential preflight probe (issue #399 item 3): the tests set a
// synthetic MISTRAL_API_KEY only to satisfy the provider-resolution gate and
// then assert binding / public / read-only behavior, so a real startup network
// embed (which would 401 against the live provider) adds only environment-
// dependent flakiness. The probe logic is unit-tested directly in internal/cli
// (TestProbeEmbedProvider).
func TestMain(m *testing.M) {
	if os.Getenv("DIR2MCP_SKIP_EMBED_PROBE") == "" {
		_ = os.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")
	}
	os.Exit(m.Run())
}
