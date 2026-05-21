package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestStatsJSONFlattening ensures that Stats.MarshalJSON flattens the
// embedded CorpusStats fields at the top level.  The hard-coded `expected`
// slice below also serves as a maintenance safeguard: it mirrors the list of
// JSON keys produced by the `plain` struct inside Stats.MarshalJSON.  If you
// add/remove/rename a field in Stats or CorpusStats you must update both that
// plain struct and this expected list, otherwise the test will fail.
func TestStatsJSONFlattening(t *testing.T) {
	s := Stats{
		Root:            "r",
		StateDir:        "s",
		ProtocolVersion: "v",
		// deliberately use a value with non-empty DocCounts so that the
		// test exercises the normal flattening behaviour.
		CorpusStats: CorpusStats{
			DocCounts:       map[string]int64{"a": 1},
			TotalDocs:       2,
			Scanned:         3,
			Indexed:         4,
			Skipped:         5,
			Deleted:         6,
			Representations: 7,
			ChunksTotal:     8,
			EmbeddedOK:      9,
			Errors:          10,
		},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal of json map failed: %v", err)
	}
	// ensure the embedded struct was flattened
	if _, ok := out["CorpusStats"]; ok {
		t.Fatalf("expected embedded struct to be flattened, got CorpusStats key")
	}
	expected := []string{
		"root",
		"state_dir",
		"protocol_version",
		"doc_counts",
		"total_docs",
		"scanned",
		"indexed",
		"skipped",
		"deleted",
		"representations",
		"chunks_total",
		"embedded_ok",
		"errors",
		"unknown",
	}
	for _, key := range expected {
		if _, ok := out[key]; !ok {
			t.Errorf("expected key %q in json output", key)
		}
	}

	// sanity-check: every json tag in CorpusStats should appear in the above
	// list, except for fields tagged omitempty (which legitimately don't
	// appear when their value is the zero value, as is the case for
	// failure_summary on the populated-but-summary-less fixture above).
	// This still catches developers who forget to update the expected
	// slice when adding new always-emitted stats fields.
	csTags := make(map[string]struct{})
	csType := reflect.TypeOf(CorpusStats{})
	for i := 0; i < csType.NumField(); i++ {
		tag := csType.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		omitempty := false
		for _, opt := range parts[1:] {
			if opt == "omitempty" {
				omitempty = true
				break
			}
		}
		if omitempty {
			continue
		}
		csTags[name] = struct{}{}
	}
	for tag := range csTags {
		found := false
		for _, key := range expected {
			if key == tag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("json tag %q from CorpusStats missing in expected list", tag)
		}
	}
}

// Ensure that a zero-value CorpusStats (with nil DocCounts) encodes to an
// object rather than null. This guards against clients breaking when they
// receive stats from components that didn't pre-populate the map.
func TestCorpusStatsMarshalNilDocCounts(t *testing.T) {
	cs := CorpusStats{} // DocCounts is nil here
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal of json map failed: %v", err)
	}
	v, ok := out["doc_counts"]
	if !ok {
		t.Fatalf("json output missing doc_counts key")
	}
	if v == nil {
		t.Fatalf("expected doc_counts object, got null")
	}
	if _, ok := v.(map[string]interface{}); !ok {
		t.Fatalf("expected doc_counts to be object, got %T", v)
	}
}

func TestStatsMarshalNilDocCounts(t *testing.T) {
	s := Stats{Root: "r", StateDir: "s", ProtocolVersion: "v"}
	// CorpusStats is zero value; DocCounts == nil
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("stats marshal failed: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal of json map failed: %v", err)
	}
	// metadata should still be present
	for _, key := range []string{"root", "state_dir", "protocol_version"} {
		if _, ok := out[key]; !ok {
			t.Errorf("expected metadata key %q in json output", key)
		}
	}
	// doc_counts should be an object, not null
	v, ok := out["doc_counts"]
	if !ok {
		t.Fatalf("json output missing doc_counts key")
	}
	if v == nil {
		t.Fatalf("expected doc_counts object, got null")
	}
	if _, ok := v.(map[string]interface{}); !ok {
		t.Fatalf("expected doc_counts to be object, got %T", v)
	}
}
