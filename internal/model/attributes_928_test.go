package model

import "testing"

// NormalizeAttributes must be a pure function of its input: a raw-key
// collision resolved by Go's map iteration order would flap the derivation
// hash, the persisted extra_json and the filter's answer across identical
// inputs. The documented winner is the lexically greatest raw key.
func TestNormalizeAttributesCollisionIsDeterministic(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := NormalizeAttributes(map[string]string{
			" inning ": "6",
			"inning":   "7",
			"inning ":  "8",
		})
		if len(got) != 1 || got["inning"] != "8" {
			t.Fatalf("run %d: got %v, want inning=8 (lexically greatest raw key wins)", i, got)
		}
	}
}

func TestAttributeKeyConflict(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  bool
	}{
		{"nil", nil, false},
		{"distinct keys", map[string]string{"inning": "8", "half": "top"}, false},
		{"collision, different values", map[string]string{"inning": "8", " inning ": "9"}, true},
		{"collision, same value after trim", map[string]string{"inning": "8", " inning ": " 8 "}, false},
		{"blank value cannot conflict", map[string]string{"inning": "8", " inning ": "  "}, false},
		{"blank key cannot conflict", map[string]string{"  ": "8", " ": "9"}, false},
	}
	for _, tc := range cases {
		if got := AttributeKeyConflict(tc.attrs); got != tc.want {
			t.Errorf("%s: AttributeKeyConflict = %v, want %v", tc.name, got, tc.want)
		}
	}
}
