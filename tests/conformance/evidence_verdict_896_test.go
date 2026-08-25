package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

// Issues #896 and #785, spec 0.55.0: the `evidence` verdict. These tests pin
// the wire contract against the schema in the PINNED dirstral-spec submodule,
// the stats_canonical_schema_850 idiom, so drift in either direction fails.

// canonicalHitPropertyNames reads the canonical common.json Hit's property
// names from the submodule.
func canonicalHitPropertyNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "common.json"))
	if err != nil {
		t.Fatalf("read canonical common.json (run: git submodule update --init): %v", err)
	}
	var doc struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical common.json: %v", err)
	}
	var hit map[string]interface{}
	if err := json.Unmarshal(doc.Definitions["Hit"], &hit); err != nil {
		t.Fatalf("decode canonical Hit: %v", err)
	}
	return schemaPropertyNames(t, hit, "canonical common.json Hit")
}

// evidenceVocabulary896 is the closed verdict enum of spec 0.55.0.
var evidenceVocabulary896 = map[string]bool{
	"strong": true, "sufficient": true, "insufficient": true, "unknown": true,
}

// TestHit896_ServedHitMatchesTheCanonicalShape pins served == canonical for the
// Hit property list, which is what makes `evidence` legal wire: both objects
// are closed, so an undeclared field on either side is the #850 failure.
func TestHit896_ServedHitMatchesTheCanonicalShape(t *testing.T) {
	t.Parallel()
	schemas := toolsListSchemas(t)
	search, ok := schemas[protocol.ToolNameSearch].(map[string]interface{})
	if !ok {
		t.Fatal("tools/list advertises no search outputSchema")
	}
	defs, _ := search["definitions"].(map[string]interface{})
	served, ok := defs["Hit"].(map[string]interface{})
	if !ok {
		t.Fatalf("served search outputSchema declares no Hit definition: %v", defs)
	}
	want := canonicalHitPropertyNames(t)
	got := schemaPropertyNames(t, served, "served Hit definition")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("served Hit properties = %v, canonical = %v", got, want)
	}
	for _, name := range []string{"evidence"} {
		found := false
		for _, p := range want {
			if p == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("canonical Hit does not declare %q; the submodule pin predates spec 0.55.0", name)
		}
	}
}

// TestSearchAndAsk896_ServedVerdictsStayInsideTheVocabulary is the end-to-end
// pin: every hit a live server serves carries an evidence verdict, the verdict
// is inside the closed vocabulary, and the ask result carries the aggregate.
// Which specific verdict applies is pinned at the retrieval level
// (tests/retrieval/evidence_verdict_896_test.go), where the score path is
// controlled; here the harness retrieves through its real vector path, so this
// test asserts presence and vocabulary rather than a particular value.
func TestSearchAndAsk896_ServedVerdictsStayInsideTheVocabulary(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer861(t, sources861)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameSearch,
		`{"query":"home run","k":20}`)
	hits, ok := structured["hits"].([]interface{})
	if !ok || len(hits) == 0 {
		t.Fatalf("no hits: %#v", structured["hits"])
	}
	for i, raw := range hits {
		hit, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("hit[%d] is not an object", i)
		}
		verdict, present := hit["evidence"].(string)
		if !present {
			t.Fatalf("hit[%d] carries no evidence verdict; spec 0.55.0 exposure missing: %#v", i, hit)
		}
		if !evidenceVocabulary896[verdict] {
			t.Fatalf("hit[%d] evidence = %q, outside the closed vocabulary", i, verdict)
		}
	}

	asked := callToolStructured856(t, srv, cfg, protocol.ToolNameAsk,
		`{"question":"who hit a home run?","k":20}`)
	verdict, present := asked["evidence"].(string)
	if !present {
		t.Fatalf("ask result carries no evidence verdict; spec 0.55.0 aggregate missing: %v", asked["evidence"])
	}
	if !evidenceVocabulary896[verdict] {
		t.Fatalf("ask evidence = %q, outside the closed vocabulary", verdict)
	}
}
