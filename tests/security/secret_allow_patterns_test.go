package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

// SPEC §7.2 (spec 0.93.0): the generic bearer-token rule stays broad, and
// security.secret_allow_patterns releases known benign keys. An allow match
// cancels only a generic token match that it covers; a match of any other rule
// is never cancelled.

// fakeRun returns n characters from [A-Za-z0-9]. The values are obviously
// synthetic; prefixes below are split so a repository secret scanner never
// sees a credential-shaped literal in this file.
func fakeRun(n int) string {
	const alphabet = "Xy7Zq9Wv"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[i%len(alphabet)]
	}
	return string(b)
}

func fakeUpperRun(n int) string {
	const alphabet = "Q7Z2X9W4"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[i%len(alphabet)]
	}
	return string(b)
}

func defaultMatcher(t *testing.T) *secretmatch.Matcher {
	t.Helper()
	cfg := config.Default()
	m, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		t.Fatalf("compile the defaults: %v", err)
	}
	return m
}

// TestSecretDefaults_SpecFixtures runs the normative regression fixtures of
// SPEC §7.2 against the shipped defaults and the matcher every screen uses.
func TestSecretDefaults_SpecFixtures(t *testing.T) {
	m := defaultMatcher(t)
	tok := "TO" + "KEN"
	redirect := "https://www.youtube.com/redirect?event=video_description&redir_" + strings.ToLower(tok) + "="

	mustHit := map[string]string{
		"GITHUB_TOKEN":             "GITHUB_" + tok + "=" + fakeRun(40),
		"bare token":               strings.ToLower(tok) + ": " + fakeRun(24),
		"PASSWORD_RESET_TOKEN":     "PASSWORD_RESET_" + tok + "=" + fakeRun(24),
		"VERCEL_TOKEN":             "VERCEL_" + tok + "=" + fakeRun(24),
		"id_token":                 "id_" + strings.ToLower(tok) + ": " + fakeRun(30),
		"OPENAI_API_KEY":           "OPENAI_API_KEY=s" + "k-proj-" + fakeRun(40),
		"redirect plus credential": redirect + fakeRun(24) + " GITHUB_" + tok + "=" + fakeRun(40),
		"credential after ; in the redirect value": redirect + "abc;GITHUB_" + tok + "=" + fakeRun(40),
		"credential after ? in the redirect value": redirect + "abc?GITHUB_" + tok + "=" + fakeRun(40),
		"credential after : in the redirect value": redirect + "abc,GITHUB_" + tok + ":" + fakeRun(40),
		"match that starts inside a cancelled one": redirect + strings.Repeat("A", 20) + "GITHUB_" + tok + "=" + strings.Repeat("B", 20),
		"AWS key inside the redirect value":        redirect + "AK" + "IA" + fakeUpperRun(16),
		"pagination name on another host":          "https://example.org/list?page_" + strings.ToLower(tok) + "=" + fakeRun(24),
		"redir_token name on another host":         "https://example.org/x?redir_" + strings.ToLower(tok) + "=" + fakeRun(24),
	}
	for name, sample := range mustHit {
		if !m.MatchString(sample) {
			t.Errorf("must hit %s: %q did not match", name, sample)
		}
	}

	mustNotHit := map[string]string{
		"redirect link": redirect + "QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4&q=https%3A%2F%2Fexample.org",
		// A base64 value with '=' padding: the allow value stops at '=', and so
		// does the generic token match, so the allow match still covers it.
		"redirect link with base64 padding": redirect + "QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4MTIz==&q=https%3A%2F%2Fexample.org",
		"URL slug":                          "https://example.org/news/krasnoyar" + "s" + "k-podpisanie-dogovorov-o-sotrudnichestve",
	}
	for name, sample := range mustNotHit {
		if m.MatchString(sample) {
			t.Errorf("must not hit %s: %q matched", name, sample)
		}
	}
}

// TestSecretDefaults_ProviderTokenNames pins that the broad rule catches the
// credential keys of providers that a key-name list would not name.
func TestSecretDefaults_ProviderTokenNames(t *testing.T) {
	m := defaultMatcher(t)
	names := []string{
		"HUGGINGFACE_TOKEN", "OPENAI_TOKEN", "DISCORD_TOKEN", "TELEGRAM_TOKEN",
		"DIGITALOCEAN_TOKEN", "DO_TOKEN", "CF_TOKEN", "VERCEL_TOKEN", "CODECOV_TOKEN",
		"DOCKERHUB_TOKEN", "DOCKER_TOKEN", "LINEAR_TOKEN", "NOTION_TOKEN", "FIGMA_TOKEN",
		"ASANA_TOKEN", "JIRA_TOKEN", "CIRCLE_TOKEN", "TRAVIS_TOKEN", "AZURE_DEVOPS_TOKEN",
		"GOOGLE_TOKEN", "ROUTER_TOKEN", "id_token", "MAPBOX_TOKEN", "CONSUL_TOKEN",
		"NOMAD_TOKEN", "TF_TOKEN", "TFE_TOKEN", "SONAR_TOKEN", "HUGGING_FACE_HUB_TOKEN",
	}
	if len(names) != 29 {
		t.Fatalf("name list holds %d names, want 29", len(names))
	}
	for _, name := range names {
		for _, sample := range []string{name + "=" + fakeRun(32), name + ": " + fakeRun(32)} {
			if !m.MatchString(sample) {
				t.Errorf("default secret patterns miss %q", sample)
			}
		}
	}
}

// TestSecretAllow_CancelsOnlyTheGenericTokenRule pins the cover rule with an
// operator allow pattern: it releases a generic token match that it covers,
// and it never releases a match of another rule.
func TestSecretAllow_CancelsOnlyTheGenericTokenRule(t *testing.T) {
	tok := "to" + "ken"
	m, err := secretmatch.Compile(
		[]string{secretmatch.GenericTokenPattern, `BLUEHERON-[0-9]{4}`},
		[]string{`feature_` + tok + `=[A-Za-z0-9]+`, `BLUEHERON-[0-9]{4}`},
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"covered generic match is cancelled", "feature_" + tok + "=" + fakeRun(30), false},
		{"uncovered generic match stays", "session_" + tok + "=" + fakeRun(30), true},
		{"operator rule is never cancelled", "codename BLUEHERON-2026", true},
		{"one covered and one uncovered match", "feature_" + tok + "=" + fakeRun(30) + "\nsession_" + tok + "=" + fakeRun(30), true},
		{"generic match that runs past the allow match", "feature_" + tok + "=" + fakeRun(30) + "-" + fakeRun(4), true},
	}
	for _, tc := range cases {
		if got := m.MatchString(tc.text); got != tc.want {
			t.Errorf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}

	none, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !none.MatchString("feature_" + tok + "=" + fakeRun(30)) {
		t.Error("an empty allow list cancelled a match")
	}
}

// TestSecretAllow_InvalidPatternIsAnError pins that the matcher rejects an
// allow pattern that does not compile and names the list.
func TestSecretAllow_InvalidPatternIsAnError(t *testing.T) {
	_, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, []string{`(unclosed`})
	if err == nil || !strings.Contains(err.Error(), "allow pattern") {
		t.Fatalf("err = %v, want a compile error that names the allow list", err)
	}
}

// TestSecretPolicy_Identity pins the fingerprint contract: the order of the
// patterns and blank entries do not change it, any change to either list does,
// and the two lists never share an encoding.
func TestSecretPolicy_Identity(t *testing.T) {
	identity := func(secret, allow []string) string {
		t.Helper()
		m, err := secretmatch.Compile(secret, allow)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return m.Policy().Identity()
	}
	base := identity([]string{`alpha[0-9]+`, `beta`}, []string{`gamma`})
	if !strings.HasPrefix(base, "v2:") {
		t.Fatalf("identity %q has no v2 prefix", base)
	}
	if got := identity([]string{`beta`, "  ", `alpha[0-9]+`}, []string{`gamma`}); got != base {
		t.Errorf("reordered list identity = %q, want %q", got, base)
	}
	for name, got := range map[string]string{
		"secret removed": identity([]string{`alpha[0-9]+`}, []string{`gamma`}),
		"secret added":   identity([]string{`alpha[0-9]+`, `beta`, `delta`}, []string{`gamma`}),
		"secret edited":  identity([]string{`alpha[0-9]*`, `beta`}, []string{`gamma`}),
		"allow removed":  identity([]string{`alpha[0-9]+`, `beta`}, nil),
		"allow added":    identity([]string{`alpha[0-9]+`, `beta`}, []string{`gamma`, `delta`}),
		"moved list":     identity([]string{`alpha[0-9]+`, `beta`, `gamma`}, nil),
	} {
		if got == base {
			t.Errorf("%s did not change the identity", name)
		}
	}
	if identity([]string{"alpha", "beta"}, nil) == identity([]string{"alpha\nbeta"}, nil) {
		t.Error("two patterns and one pattern that joins them with a newline share an identity")
	}
}

// TestSecretPolicy_Narrowing pins which changes count as "can only match
// less". Those need no read of indexed documents and hide nothing.
func TestSecretPolicy_Narrowing(t *testing.T) {
	cfg := config.Default()
	shipped, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defaults := shipped.Policy()
	old := secretmatch.PreSpec0930DefaultPolicy()

	inDefaults := map[string]bool{}
	for _, p := range defaults.Secret {
		inDefaults[p] = true
	}
	inOld := map[string]bool{}
	for _, p := range old.Secret {
		inOld[p] = true
	}
	for narrowed, previous := range secretmatch.NarrowedSecretPatterns() {
		if !inDefaults[narrowed] {
			t.Errorf("narrowing %q is not a shipped default", narrowed)
		}
		if !inOld[previous] {
			t.Errorf("narrowed pattern %q is not a pre-0.93.0 default", previous)
		}
	}
	if !inDefaults[secretmatch.GenericTokenPattern] {
		t.Error("the generic token rule is not a shipped default")
	}

	if !secretmatch.IsNarrower(old, defaults) {
		t.Error("the spec 0.93.0 defaults must count as narrower than the pre-0.93.0 defaults")
	}
	with := func(p secretmatch.Policy, secret, allow []string) secretmatch.Policy {
		return secretmatch.Policy{
			Secret: append(append([]string{}, p.Secret...), secret...),
			Allow:  append(append([]string{}, p.Allow...), allow...),
		}
	}
	if !secretmatch.IsNarrower(defaults, secretmatch.Policy{Secret: defaults.Secret[1:], Allow: defaults.Allow}) {
		t.Error("removing a secret pattern must count as narrower")
	}
	if !secretmatch.IsNarrower(defaults, with(defaults, nil, []string{`feature_token=[a-z]+`})) {
		t.Error("adding an allow pattern must count as narrower")
	}
	if secretmatch.IsNarrower(defaults, with(defaults, []string{`BLUEHERON-[0-9]+`}, nil)) {
		t.Error("adding a secret pattern must not count as narrower")
	}
	if secretmatch.IsNarrower(defaults, secretmatch.Policy{Secret: defaults.Secret}) {
		t.Error("removing an allow pattern must not count as narrower")
	}
	if secretmatch.IsNarrower(defaults, old) {
		t.Error("going back to the boundary-less defaults must not count as narrower")
	}
	custom := with(defaults, []string{`BLUEHERON-[0-9]+`}, nil)
	stricter := with(defaults, []string{`BLUEHERON-[0-9]{4}`}, nil)
	if secretmatch.IsNarrower(custom, stricter) {
		t.Error("an undeclared stricter edit must count as broader")
	}
}

// TestSecretPolicy_NeedsIndexedReevaluation pins the one decision that ingest
// (its reset) and retrieval (its serve-time transition check) share.
func TestSecretPolicy_NeedsIndexedReevaluation(t *testing.T) {
	cfg := config.Default()
	shipped, err := secretmatch.Compile(cfg.SecretPatterns, cfg.SecretAllowPatterns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defaults := shipped.Policy()
	custom := secretmatch.Policy{Secret: append(append([]string{}, defaults.Secret...), `BLUEHERON-[0-9]+`), Allow: defaults.Allow}
	narrower := secretmatch.Policy{Secret: defaults.Secret, Allow: append(append([]string{}, defaults.Allow...), `feature_token=[a-z]+`)}
	rec := func(p secretmatch.Policy) secretmatch.Recorded {
		return secretmatch.Recorded{Identity: p.Identity(), Policy: p, Known: true}
	}
	cases := []struct {
		name     string
		recorded secretmatch.Recorded
		active   secretmatch.Policy
		want     bool
	}{
		{"same policy", rec(defaults), defaults, false},
		{"added secret pattern", rec(defaults), custom, true},
		{"removed secret pattern", rec(custom), defaults, false},
		{"added allow pattern", rec(defaults), narrower, false},
		{"removed allow pattern", rec(narrower), defaults, true},
		{"unrecorded corpus, no provenance, shipped defaults", secretmatch.Recorded{}, defaults, true},
		{"unrecorded corpus, no provenance, custom policy", secretmatch.Recorded{}, custom, true},
		{"unrecorded corpus, provenance of the old defaults", secretmatch.Recorded{Policy: secretmatch.PreSpec0930DefaultPolicy(), Known: true}, defaults, false},
		{"unrecorded corpus, provenance of a custom list", secretmatch.Recorded{Policy: secretmatch.Policy{Secret: []string{`BLUEHERON-[0-9]+`}}, Known: true}, defaults, true},
		{"recorded lists unreadable", secretmatch.Recorded{Identity: "v2:other"}, defaults, true},
	}
	for _, tc := range cases {
		if got := secretmatch.NeedsIndexedReevaluation(tc.recorded, tc.active); got != tc.want {
			t.Errorf("%s: NeedsIndexedReevaluation = %v, want %v", tc.name, got, tc.want)
		}
	}
}
