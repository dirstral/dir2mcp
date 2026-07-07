package ingest

import (
	"strings"
	"testing"
)

// TestRedactHighConfidenceCredentials pins the single shared redactor used by
// both the MCP recent_failures surface and the CLI `status` coverage report
// (#414 review consolidation). It must cover every high-confidence shape from
// both former redactors, and must NOT over-redact benign prose (the generic
// `keyword: value` shape was deliberately left out to preserve §15.6
// actionability).
func TestRedactHighConfidenceCredentials(t *testing.T) {
	redacted := []struct {
		name string
		in   string
	}{
		{"bearer", "auth failed: Bearer abcdefghijklmnopqrstuvwxyz0123456789"},
		{"aws-akia", "creds AKIA1234567890ABCDEF rejected"},
		{"aws-asia", "temp creds ASIA1234567890ABCDEF rejected"},
		{"stripe-sk-live", "key sk_live_0123456789ABCDEFghij bad"},
		{"openai-sk", "key sk-proj-0123456789ABCDEFghij bad"},
		{"github-pat", "token ghp_0123456789abcdefABCDEF0123 denied"},
		{"slack", "hook xoxb-0123456789-abcdefABCDEF expired"},
		{"jwt", "jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdef bad"},
	}
	for _, c := range redacted {
		got := RedactHighConfidenceCredentials(c.in)
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s: expected redaction, got %q", c.name, got)
		}
	}

	// The high-confidence base must NOT redact generic `keyword value` prose (no
	// `:`/`=` assignment) — that preserves §15.6 actionability on the MCP surface.
	keep := []string{
		"the token expired 3 hours ago",
		"password authentication failed for user alice",
		"connection refused after 3 retries",
	}
	for _, msg := range keep {
		if got := RedactHighConfidenceCredentials(msg); got != msg {
			t.Errorf("base over-redacted benign message %q -> %q", msg, got)
		}
	}

	if got := RedactHighConfidenceCredentials(""); got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

// TestRedactCredentialsForDisplay pins that the CLI display variant additionally
// scrubs generic `keyword=value` credential assignments (a real terminal leak),
// while the MCP high-confidence base leaves them for actionability.
func TestRedactCredentialsForDisplay(t *testing.T) {
	leak := "auth failed password=hunter2supersecret"
	if got := RedactCredentialsForDisplay(leak); strings.Contains(got, "hunter2supersecret") {
		t.Errorf("display variant leaked credential: %q", got)
	}
	// Same input on the base (MCP) surface is intentionally left actionable.
	if got := RedactHighConfidenceCredentials(leak); !strings.Contains(got, "hunter2supersecret") {
		t.Errorf("base unexpectedly redacted generic keyword=value: %q", got)
	}
	// The display variant still inherits every high-confidence shape.
	if got := RedactCredentialsForDisplay("key ghp_0123456789abcdefABCDEF0123 denied"); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("display variant missed a high-confidence shape: %q", got)
	}
}
