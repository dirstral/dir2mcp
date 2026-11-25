package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// Person id normalization and name folds (SPEC §7.11.4, conformance case 6).

func mustFolds(t *testing.T, list ...string) editorial.Folds {
	t.Helper()
	folds, err := editorial.ParseFolds(list)
	if err != nil {
		t.Fatalf("ParseFolds(%q): %v", list, err)
	}
	return folds
}

func TestPersonID_DefaultFoldsNothing(t *testing.T) {
	none := editorial.Folds{}
	if got := editorial.PersonID("  ПЁТР \t Ёлкин ", none); got != "пётр ёлкин" {
		t.Fatalf("PersonID = %q, want %q", got, "пётр ёлкин")
	}
	if a, b := editorial.PersonID("Пётр Ёлкин", none), editorial.PersonID("Петр Елкин", none); a == b {
		t.Fatalf("with no fold the two spellings share the id %q", a)
	}
	// U+0130 lower-cases to U+0069 U+0307 (full case mapping), and NFC keeps it.
	if got := editorial.PersonID("İ", none); got != "i̇" {
		t.Fatalf("PersonID(U+0130) = %q, want i U+0307", got)
	}
	// Decomposed input composes: е U+0308 is ё.
	if got := editorial.PersonID("Пётр", none); got != "пётр" {
		t.Fatalf("PersonID(decomposed) = %q, want пётр", got)
	}
}

func TestPersonID_FoldMergesSpellings(t *testing.T) {
	folds := mustFolds(t, "ё=е")
	a := editorial.PersonID("Пётр Ёлкин", folds)
	b := editorial.PersonID("Петр Елкин", folds)
	if a != "петр елкин" || a != b {
		t.Fatalf("with ё=е the ids are %q and %q, want both петр елкин", a, b)
	}
	// ё before U+0306 folds to е, and NFC then composes е U+0306 into U+04D7.
	if got := editorial.PersonID("ё̆", folds); got != "ӗ" {
		t.Fatalf("PersonID(ё U+0306) = %q, want U+04D7", got)
	}
}

func TestPersonID_RepeatLoopReachesAFixedPoint(t *testing.T) {
	folds := mustFolds(t, "ъ=е", "ё=е")
	if got := editorial.PersonID("ъ̈", folds); got != "е" {
		t.Fatalf("PersonID(ъ U+0308) = %q, want е (step 6)", got)
	}
}

func TestPersonID_IsIdempotent(t *testing.T) {
	inputs := []string{"Пётр Ёлкин", "  A  B ", "İstanbul", "ъ̈", "ё̆", "Ǆ"}
	for _, folds := range []editorial.Folds{{}, mustFolds(t, "ё=е"), mustFolds(t, "ъ=е", "ё=е")} {
		for _, in := range inputs {
			once := editorial.PersonID(in, folds)
			if twice := editorial.PersonID(once, folds); twice != once {
				t.Errorf("PersonID not idempotent for %q with %v: %q then %q", in, folds.Entries(), once, twice)
			}
		}
	}
}

func TestParseFolds_InvalidLists(t *testing.T) {
	cases := map[string][]string{
		"chain":                  {"a=b", "b=c"},
		"to changed by lower":    {"ё=Е"},
		"white space side":       {"x= "},
		"not two code points":    {"ab=c"},
		"no equals sign":         {"a-b"},
		"repeated from":          {"a=b", "a=c"},
		"combining mark side":    {"a=́"},
		"from changed by NFC":    {"Å=a"},
		"from changed by lower":  {"A=a"},
		"empty entry":            {""},
		"white space from side":  {" =a"},
		"chain in reverse order": {"b=c", "a=b"},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := editorial.ParseFolds(list); err == nil {
				t.Fatalf("ParseFolds(%q) accepted an invalid list", list)
			}
		})
	}
	for _, valid := range [][]string{nil, {}, {"ё=е"}, {"ъ=е", "ё=е"}} {
		if _, err := editorial.ParseFolds(valid); err != nil {
			t.Errorf("ParseFolds(%q): %v", valid, err)
		}
	}
}

func TestFolds_EntriesAreOrderIndependent(t *testing.T) {
	a := mustFolds(t, "ъ=е", "ё=е")
	b := mustFolds(t, "ё=е", "ъ=е")
	if !a.Equal(b) {
		t.Fatalf("the same folds in another order differ: %v vs %v", a.Entries(), b.Entries())
	}
	if got := (editorial.Folds{}).Entries(); got == nil || len(got) != 0 {
		t.Fatalf("empty folds record %#v, want an empty non-nil list", got)
	}
}
