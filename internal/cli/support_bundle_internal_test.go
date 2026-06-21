package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRedactBundleSecrets verifies credential material is masked before it can
// reach the shareable support bundle (issue #358).
func TestRedactBundleSecrets(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustNot  string
		mustHave string
	}{
		{
			name:     "bearer token",
			in:       "calling with Bearer abc123.def456.ghi789 now",
			mustNot:  "abc123.def456.ghi789",
			mustHave: "Bearer [REDACTED]",
		},
		{
			name:     "authorization header",
			in:       `{"Authorization":"Bearer sk_live_supersecretvalue"}`,
			mustNot:  "sk_live_supersecretvalue",
			mustHave: "[REDACTED]",
		},
		{
			name:     "token query param",
			in:       "http://127.0.0.1:8080/mcp?token=supersecretquerytoken&x=1",
			mustNot:  "supersecretquerytoken",
			mustHave: "token=[REDACTED]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactBundleSecrets(tc.in)
			if strings.Contains(got, tc.mustNot) {
				t.Fatalf("secret leaked through redaction: %q still in %q", tc.mustNot, got)
			}
			if !strings.Contains(got, tc.mustHave) {
				t.Fatalf("expected %q in redacted output, got %q", tc.mustHave, got)
			}
		})
	}
}

// TestMarshalDaemonLivenessJSON_NoConnection records connection_present=false
// and reachable=false when no connection.json exists.
func TestMarshalDaemonLivenessJSON_NoConnection(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	out, err := marshalDaemonLivenessJSON(cfg)
	if err != nil {
		t.Fatalf("marshalDaemonLivenessJSON: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["connection_present"] != false {
		t.Errorf("connection_present = %v, want false", got["connection_present"])
	}
	if got["reachable"] != false {
		t.Errorf("reachable = %v, want false", got["reachable"])
	}
}

// TestMarshalDaemonLivenessJSON_RedactsToken verifies that when connection.json
// is present the bearer token never appears in daemon.json: the URL is redacted
// and only header names (not values) are recorded.
func TestMarshalDaemonLivenessJSON_RedactsToken(t *testing.T) {
	const token = "supersecrettoken-должен-не-течь-1234567890"
	stateDir := t.TempDir()
	conn := connectionPayload{
		Transport:   "http",
		URL:         "http://127.0.0.1:59999/mcp?token=" + token,
		Headers:     map[string]string{"Authorization": "Bearer " + token},
		TokenSource: "file",
	}
	raw, err := json.Marshal(conn)
	if err != nil {
		t.Fatalf("marshal conn: %v", err)
	}
	if err := os.WriteFile(connectionFilePath(stateDir), raw, 0o600); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}

	out, err := marshalDaemonLivenessJSON(config.Config{StateDir: stateDir})
	if err != nil {
		t.Fatalf("marshalDaemonLivenessJSON: %v", err)
	}
	if strings.Contains(string(out), token) {
		t.Fatalf("token leaked into daemon.json: %s", out)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["connection_present"] != true {
		t.Errorf("connection_present = %v, want true", got["connection_present"])
	}
	keys, ok := got["header_keys"].([]interface{})
	if !ok || len(keys) != 1 || keys[0] != "Authorization" {
		t.Errorf("header_keys = %v, want [Authorization] (names only)", got["header_keys"])
	}
	if url, _ := got["url"].(string); !strings.Contains(url, "[REDACTED]") {
		t.Errorf("url = %q, want it redacted", url)
	}
}
