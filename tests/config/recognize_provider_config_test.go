package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestValidateRecognizeProvider exercises the design-0004 recognition binding
// validation (Config.Validate → validateRecognizeProvider): the only providers
// are off (default) and serve; serve requires an http(s) base URL with a host
// and MUST NOT embed credentials; a serve_command without provider=serve is a
// misconfiguration.
func TestValidateRecognizeProvider(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		serveURL    string
		serveCmd    string
		wantErr     bool
		errContains string
	}{
		{name: "default off passes", provider: "off"},
		{name: "empty treated as off", provider: ""},
		{name: "serve with http url passes", provider: "serve", serveURL: "http://127.0.0.1:8099"},
		{name: "serve with https url passes", provider: "serve", serveURL: "https://recognizer.internal:443"},
		{
			name: "serve with embedded credentials rejected", provider: "serve",
			serveURL: "http://user:s3cr3t@127.0.0.1:8099", wantErr: true, errContains: "must not embed credentials",
		},
		{
			name: "serve without host rejected", provider: "serve",
			serveURL: "http:///health", wantErr: true, errContains: "http(s) URL",
		},
		{
			name: "serve with non-http scheme rejected", provider: "serve",
			serveURL: "ftp://host:21", wantErr: true, errContains: "http(s) URL",
		},
		{
			name: "serve_command without serve rejected", provider: "off",
			serveCmd: "dirstral-annotator serve", wantErr: true, errContains: "recognize.provider",
		},
		{
			name: "unknown provider rejected", provider: "bogus",
			wantErr: true, errContains: "not a recognized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.RecognizeProvider = tc.provider
			cfg.RecognizeServeURL = tc.serveURL
			cfg.RecognizeServeCommand = tc.serveCmd
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				// The credential-rejection message must never echo the secret.
				if strings.Contains(err.Error(), "s3cr3t") {
					t.Fatalf("error message leaked embedded credentials: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
