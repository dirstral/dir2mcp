package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestWriteDownInfo_ServiceManagedNote pins the #434 down-vs-service
// contract: after a successful stop of a service-managed corpus, `down`
// appends an informational note (prose) and sets service_managed=true in the
// JSON payload — but never auto-uninstalls and keeps exit 0. When the corpus
// is not service-managed the note is absent and the field is false.
func TestWriteDownInfo_ServiceManagedNote(t *testing.T) {
	const note = "service-managed"

	// Text mode, service-managed: the note must appear on a "stopped" result.
	var managed bytes.Buffer
	writeDownInfo(&managed, false, "/corpus/.dir2mcp", 4242, true, "stopped", true)
	if !strings.Contains(managed.String(), note) {
		t.Errorf("expected a service-managed note, got %q", managed.String())
	}
	if !strings.Contains(managed.String(), "service uninstall") {
		t.Errorf("note should point at `service uninstall`, got %q", managed.String())
	}

	// Text mode, not service-managed: no note.
	var plain bytes.Buffer
	writeDownInfo(&plain, false, "/corpus/.dir2mcp", 4242, true, "stopped", false)
	if strings.Contains(plain.String(), note) {
		t.Errorf("did not expect a service-managed note, got %q", plain.String())
	}

	// A non-stop result (stale pid) must never carry the note even if the flag
	// were somehow set, because nothing was actually stopped.
	var stale bytes.Buffer
	writeDownInfo(&stale, false, "/corpus/.dir2mcp", 4242, false, "stale_pid", true)
	if strings.Contains(stale.String(), note) {
		t.Errorf("stale-pid result should not carry a stop note, got %q", stale.String())
	}

	// JSON mode carries service_managed in the payload (both values).
	for _, want := range []bool{true, false} {
		var buf bytes.Buffer
		writeDownInfo(&buf, true, "/corpus/.dir2mcp", 4242, true, "stopped", want)
		var payload struct {
			Stopped        bool `json:"stopped"`
			ServiceManaged bool `json:"service_managed"`
		}
		if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
			t.Fatalf("decode down JSON: %v raw=%s", err, buf.String())
		}
		if payload.ServiceManaged != want {
			t.Errorf("service_managed = %v, want %v", payload.ServiceManaged, want)
		}
		if !payload.Stopped {
			t.Errorf("stopped should be true")
		}
	}
}
