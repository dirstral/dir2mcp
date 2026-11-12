package retrieval

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestDefaultSecretPatternLiterals_MatchConfigDefaults pins the retrieval
// baseline to the shipped config defaults. open_file applies this baseline on
// top of any caller list, so a stale copy here would refuse a document that
// ingest indexed under the current defaults (spec 0.93.0).
func TestDefaultSecretPatternLiterals_MatchConfigDefaults(t *testing.T) {
	defaults := make(map[string]struct{}, len(config.Default().SecretPatterns))
	for _, pattern := range config.Default().SecretPatterns {
		defaults[pattern] = struct{}{}
	}
	if len(defaultSecretPatternLiterals) == 0 {
		t.Fatal("retrieval baseline secret patterns are empty")
	}
	for _, literal := range defaultSecretPatternLiterals {
		if _, ok := defaults[literal]; !ok {
			t.Errorf("retrieval baseline pattern %q is not in config.Default().SecretPatterns", literal)
		}
	}
}
