package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// anyMatch reports whether any of the compiled default secret patterns matches s.
func anyMatch(t *testing.T, patterns []string, s string) bool {
	t.Helper()
	res, err := ingest.CompileSecretPatterns(patterns)
	if err != nil {
		t.Fatalf("CompileSecretPatterns: %v", err)
	}
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// TestDefaultSecretPatterns_MatchProviderKeys is a regression guard for #443:
// the default scan patterns must catch real provider key formats — OpenAI/Mistral
// hyphenated "sk-..." keys and "PROVIDER_API_KEY=<value>" assignments — which the
// previous "sk_"/"api_" patterns missed entirely.
func TestDefaultSecretPatterns_MatchProviderKeys(t *testing.T) {
	patterns := config.Default().SecretPatterns

	secrets := []string{
		"sk-proj-abcdEFGH1234ijklMNOP5678qrstUVWX",
		"sk-abcdEFGH1234ijklMNOP5678qrstUVWXyz90",
		"MISTRAL_API_KEY=abcdEFGHijkl1234mnop5678QRST9012",
		"COHERE_API_KEY: 0123456789abcdefABCDEF9876543210",
		"GEMINI_API_KEY=AIzaSyA0123456789abcdefghijKLMNOP",
	}
	for _, s := range secrets {
		if !anyMatch(t, patterns, s) {
			t.Errorf("default secret patterns should match credential %q but did not", s)
		}
	}
}

// TestDefaultSecretPatterns_NoProseFalsePositives ensures the broadened patterns
// stay surgical: ordinary documentation prose must not trip the scanner.
func TestDefaultSecretPatterns_NoProseFalsePositives(t *testing.T) {
	patterns := config.Default().SecretPatterns

	prose := []string{
		"The quick brown fox jumps over the lazy dog near the river.",
		"Please ask the on-call engineer to rotate the deployment next week.",
		"Set your API key by editing the configuration file in the dashboard.",
		"This sentence mentions sk and api but contains no real credentials.",
	}
	for _, s := range prose {
		if anyMatch(t, patterns, s) {
			t.Errorf("default secret patterns false-positived on prose %q", s)
		}
	}
}
