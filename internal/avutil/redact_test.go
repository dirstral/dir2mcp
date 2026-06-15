package avutil

import (
	neturl "net/url"
	"strings"
	"testing"
)

// A presigned-URL-shaped value whose query carries the credential/signature that
// must never reach an error message, log line, or persisted failure reason.
const signedURL = "https://my-bucket.s3.amazonaws.com/media/clip.mp4" +
	"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
	"&X-Amz-Credential=AKIAEXAMPLE%2F20260615%2Fus-east-1%2Fs3%2Faws4_request" +
	"&X-Amz-Date=20260615T000000Z&X-Amz-Expires=900" +
	"&X-Amz-Signature=deadbeefcafef00dsecretsignature" +
	"&X-Amz-SignedHeaders=host"

// secretTokens are substrings that, if present in any redacted output, mean the
// presigned URL's credentials leaked.
var secretTokens = []string{"X-Amz-Signature", "deadbeefcafef00dsecretsignature", "X-Amz-Credential", "AKIAEXAMPLE"}

func assertNoSecret(t *testing.T, label, out string) {
	t.Helper()
	for _, tok := range secretTokens {
		if strings.Contains(out, tok) {
			t.Errorf("%s leaked secret token %q in output: %q", label, tok, out)
		}
	}
}

// TestRedactInput_StripsPresignedQuery pins the #1 security property of issue
// #243: a presigned URL's credential-bearing query is removed before the value
// can appear in any error or log line, while the harmless object identity
// (scheme + host + path) is kept for diagnostics.
func TestRedactInput_StripsPresignedQuery(t *testing.T) {
	out := redactInput(signedURL)
	assertNoSecret(t, "redactInput", out)
	if !strings.HasPrefix(out, "https://my-bucket.s3.amazonaws.com/media/clip.mp4") {
		t.Errorf("redactInput dropped the diagnostic object identity: %q", out)
	}
	if strings.Contains(out, "?") {
		t.Errorf("redactInput kept a query string: %q", out)
	}
}

// TestRedactInput_LeavesLocalPaths confirms local filesystem paths (the
// historical ExtractSegment input) are returned verbatim, so the local path's
// error messages are unchanged.
func TestRedactInput_LeavesLocalPaths(t *testing.T) {
	for _, p := range []string{"/var/media/clip.mp4", "relative/clip.mp3", ""} {
		if got := redactInput(p); got != p {
			t.Errorf("redactInput(%q) = %q, want unchanged", p, got)
		}
	}
}

// TestRedactInput_UserInfoStripped confirms any userinfo (another credential
// channel) is removed.
func TestRedactInput_UserInfoStripped(t *testing.T) {
	out := redactInput("https://user:pass@host/clip.mp4?X-Amz-Signature=secret")
	if strings.Contains(out, "pass") || strings.Contains(out, "secret") {
		t.Errorf("redactInput kept credentials: %q", out)
	}
}

// TestRedactStderr_ScrubsEchoedURL pins that ffmpeg stderr which echoes the full
// signed URL or just its query string is scrubbed before being wrapped into an
// error (some ffmpeg builds print the URL in protocol errors).
func TestRedactStderr_ScrubsEchoedURL(t *testing.T) {
	u, err := neturl.Parse(signedURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := map[string]string{
		"full URL echo":    "Server returned 403 Forbidden\n" + signedURL + ": Invalid data",
		"query-only echo":  "error opening with query " + u.RawQuery,
		"no URL in stderr": "Invalid data found when processing input",
	}
	for name, stderr := range cases {
		out := redactStderr(stderr, signedURL)
		assertNoSecret(t, "redactStderr/"+name, out)
	}
}

// TestExtractSegment_InvalidRangeNeverLeaksURL drives the public guard path with
// a signed URL and an invalid range so no binary or network is needed, and
// confirms the resulting error carries no credential.
func TestExtractSegment_InvalidRangeNeverLeaksURL(t *testing.T) {
	_, err := ExtractSegmentURL(nil, signedURL, ".mp4", 10, 5) //nolint:staticcheck // nil ctx ok: guard returns before ctx use
	if err == nil {
		t.Fatal("want an invalid-range error")
	}
	assertNoSecret(t, "ExtractSegmentURL invalid-range error", err.Error())
}
