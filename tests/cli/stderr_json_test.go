package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonErrorPayloadFromStderr returns the first line of captured stderr that
// parses as a JSON object, or nil if there is none.
//
// The `up --json` error envelope is emitted as a single JSON object on its own
// line (internal/cli/app.go). stderr may ALSO carry human diagnostic lines that
// the command is entitled to print — notably the port-rebind note ("note:
// previous port (...) unavailable, binding a new one ...", internal/cli/up.go)
// when a persisted port is already bound on a shared CI runner. Parsing the
// whole stderr buffer as JSON then fails on the leading `note:` text. Scanning
// for the JSON-object line keeps the payload assertions robust to those
// non-JSON lines without weakening them (issue #615). The payload's message is
// json.Marshal-escaped, so the object never spans multiple physical lines.
func jsonErrorPayloadFromStderr(stderr string) []byte {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &probe) == nil {
			return []byte(line)
		}
	}
	return nil
}

// TestJSONErrorPayloadFromStderr pins that the payload extractor tolerates the
// exact contamination that flaked TestUpReturnsExitCode3OnIngestionFatal (#615):
// a human `note:` diagnostic line printed to stderr ahead of the JSON envelope.
func TestJSONErrorPayloadFromStderr(t *testing.T) {
	const payload = `{"error":{"code":"INGESTION_FATAL","message":"ingestion failed"},"exit_code":3}`

	cases := []struct {
		name   string
		stderr string
		want   string // "" means expect nil
	}{
		{"pure payload", payload + "\n", payload},
		{
			// The observed CI failure: a port-rebind note ahead of the payload.
			name:   "note line then payload",
			stderr: "note: previous port (127.0.0.1:51606) unavailable, binding a new one: bind: address already in use\n" + payload + "\n",
			want:   payload,
		},
		{"payload then trailing note", payload + "\nnote: something after\n", payload},
		{"no json at all", "note: only diagnostics here\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonErrorPayloadFromStderr(tc.stderr)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %s", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("extracted %q, want %q", got, tc.want)
			}
			// The returned bytes must be valid, parseable JSON.
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Fatalf("extracted payload does not parse: %v", err)
			}
		})
	}
}
