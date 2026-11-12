package tests

import (
	"strings"
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

// Synthetic fixture builders for the spec 0.93.0 boundary tests. The prefixes
// are split by concatenation so a repository secret scanner never sees a
// credential-shaped literal in this file; every value is a repeated run of
// obviously fake characters.
func synthRun(n int) string {
	return repeatAlphabet("Xy7Zq9Wv", n)
}

func synthLowerRun(n int) string {
	return repeatAlphabet("x7z9q4", n)
}

func repeatAlphabet(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[i%len(alphabet)]
	}
	return string(b)
}

// TestDefaultSecretPatterns_Boundaries_0_93_0 pins the spec 0.93.0 defaults
// through the real default set and the real matcher. The must-not-match rows
// are the false positives an archive hit: 13 Word documents withheld through a
// pasted YouTube redirect link (`redir_token=`) and 4 through transliterated
// URL slugs (the `sk-` inside `krasnoyarsk-`). The must-match rows are the
// credential shapes the tightened rules must keep catching.
func TestDefaultSecretPatterns_Boundaries_0_93_0(t *testing.T) {
	patterns := config.Default().SecretPatterns
	tok := "to" + "ken"
	skDash := "s" + "k-"
	skUnder := "s" + "k_"

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		// Must NOT match.
		{"youtube redirect link", "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbWRkRThBZV" + synthRun(40) + "&q=https%3A%2F%2Fexample.org%2Fpage", false},
		{"pagination cursor", "next_page_" + tok + "=" + synthRun(30), false},
		{"csrf form field", "csrf_" + tok + "=" + synthRun(30), false},
		{"krasnoyarsk slug", "https://www.newstube.ru/media/b-elcin-i-konstantinova-v-krasnoyar" + skDash + "podpisanie-dogovorov-o-sotrudnichestve", false},
		{"baikonur slug", "https://example.org/news/bajkonur-pu" + skDash + "soyuz-28-tm-77-kosmonavty-na-orbite", false},
		{"task id with sk_ mid-word", "ta" + skUnder + synthLowerRun(32), false},
		{"task id in prose", "see job ta" + skUnder + synthLowerRun(32) + " for details", false},

		// MUST match.
		{"bare token colon", tok + ": " + synthRun(24), true},
		{"bare token equals at line start", tok + "=" + synthRun(24), true},
		{"bare token equals on a later line", "# config\n" + tok + "=" + synthRun(24), true},
		{"github token env", "GITHUB_" + strings.ToUpper(tok) + "=" + synthRun(40), true},
		{"prefixed secret token env", "MY_APP_SECRET_" + strings.ToUpper(tok) + "=" + synthRun(30), true},
		{"auth token header", "X-Auth-T" + tok[1:] + ": " + synthRun(30), true},
		{"access token query", "access_" + tok + "=" + synthRun(30), true},
		{"openai project key env", "OPENAI_API_KEY=" + skDash + "proj-" + synthRun(40), true},
		{"quoted hyphen key", `"` + skDash + synthRun(40) + `"`, true},
		{"stripe style key", "key = " + skUnder + synthLowerRun(32), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyMatch(t, patterns, tc.input); got != tc.want {
				t.Errorf("default secret patterns match %q = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
