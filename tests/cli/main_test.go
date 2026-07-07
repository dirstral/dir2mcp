package tests

import (
	"os"
	"testing"
)

// TestMain keeps the CLI integration suite hermetic: it disables the §2.5
// embedding-credential preflight probe (issue #399 item 3) for the whole
// package so `up` tests that set a synthetic MISTRAL_API_KEY to satisfy the
// provider-resolution gate do not make a real network embed at startup (which
// would 401 against the live provider and is environment-dependent). The probe
// logic itself is unit-tested directly in internal/cli
// (TestProbeEmbedProvider); these tests exercise binding / public / x402 /
// daemon behavior, not embedding, so the network probe adds only flakiness.
func TestMain(m *testing.M) {
	if os.Getenv("DIR2MCP_SKIP_EMBED_PROBE") == "" {
		_ = os.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")
	}
	os.Exit(m.Run())
}
