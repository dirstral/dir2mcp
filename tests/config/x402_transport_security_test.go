package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestX402Transport_UserinfoCountsAsCredential proves a URL carrying userinfo
// (http://user:pass@host) is treated as credentialed and rejected over plaintext
// http even on a loopback host — it must not slip through the loopback-http
// exception — and that the error never echoes the embedded credential.
func TestX402Transport_UserinfoCountsAsCredential(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		token   string
		wantErr bool
	}{
		{"https any host", "https://facilitator.example.com", "", false},
		{"http loopback no cred", "http://127.0.0.1:8080", "", false},
		{"http loopback with token", "http://127.0.0.1:8080", "secret-token", true},
		{"http loopback with userinfo", "http://user:s3cr3t@127.0.0.1:8080", "", true},
		{"http non-loopback", "http://facilitator.example.com", "", true},
		{"https with userinfo ok", "https://user:s3cr3t@facilitator.example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{}
			c.X402.FacilitatorURL = tc.url
			c.X402.FacilitatorToken = tc.token
			err := c.X402FacilitatorTransportError()
			if tc.wantErr && err == nil {
				t.Fatalf("expected transport error for case %q, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected transport error for case %q: %v", tc.name, err)
			}
			if err != nil {
				// The error must never leak embedded userinfo credentials.
				if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("transport error leaked a credential: %q", err.Error())
				}
			}
		})
	}
}
