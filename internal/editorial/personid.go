// Package editorial implements the external editorial annotation import (SPEC
// §7.11): the record shape and its validation, the staged import files, the
// records kept for rebuilds, the import report, and the mapping of one record
// to its annotations. It holds no store or index code, so the command that
// stages an import and the process that applies it share one definition.
//
// The package depends only on the standard library and golang.org/x/text, so
// the model package can use the person id normalization for the entities
// filter without an import cycle.
package editorial

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// Folds is a validated `annotations.name_folds` list (SPEC §7.11.4): single
// code point folds applied in step 3 of the person id normalization. The zero
// value folds no character, which is the default.
type Folds struct {
	byFrom map[rune]rune
	// entries holds the validated "<from>=<to>" entries, sorted, so two lists
	// with the same folds in a different order compare equal.
	entries []string
}

// ParseFolds validates a configured fold list. Each rule of SPEC §7.11.4 that
// makes an entry CONFIG_INVALID returns an error that names the entry. An
// empty or nil list is valid and folds nothing.
func ParseFolds(list []string) (Folds, error) {
	if len(list) == 0 {
		return Folds{}, nil
	}
	byFrom := make(map[rune]rune, len(list))
	for _, entry := range list {
		from, to, err := parseFoldEntry(entry)
		if err != nil {
			return Folds{}, err
		}
		if _, dup := byFrom[from]; dup {
			return Folds{}, fmt.Errorf("annotations.name_folds entry %q repeats the <from> %q", entry, string(from))
		}
		byFrom[from] = to
	}
	// Checked in list order, so the error names the same entry on every run.
	for _, entry := range list {
		runes := []rune(entry)
		if _, chained := byFrom[runes[2]]; chained && runes[2] != runes[0] {
			return Folds{}, fmt.Errorf("annotations.name_folds entry %q has a <to> that is the <from> of another entry (a chain)", entry)
		}
	}
	entries := make([]string, 0, len(byFrom))
	for from, to := range byFrom {
		entries = append(entries, string(from)+"="+string(to))
	}
	sort.Strings(entries)
	return Folds{byFrom: byFrom, entries: entries}, nil
}

// parseFoldEntry checks one "<from>=<to>" entry against the per-entry rules:
// two single code points joined by "=", neither side changed by lower case or
// NFC, and neither side white space or a combining mark.
func parseFoldEntry(entry string) (rune, rune, error) {
	if !utf8.ValidString(entry) {
		return 0, 0, fmt.Errorf("annotations.name_folds entry %q is not valid UTF-8", entry)
	}
	runes := []rune(entry)
	if len(runes) != 3 || runes[1] != '=' {
		return 0, 0, fmt.Errorf("annotations.name_folds entry %q is not two single code points joined by \"=\"", entry)
	}
	from, to := runes[0], runes[2]
	for _, side := range []rune{from, to} {
		if err := checkFoldSide(entry, side); err != nil {
			return 0, 0, err
		}
	}
	return from, to, nil
}

// checkFoldSide rejects a fold side that a later normalization step could
// change or remove, because a filter for a stored id would then miss it.
func checkFoldSide(entry string, side rune) error {
	s := string(side)
	if lowerCase(s) != s {
		return fmt.Errorf("annotations.name_folds entry %q has a side that lower case changes", entry)
	}
	if norm.NFC.String(s) != s {
		return fmt.Errorf("annotations.name_folds entry %q has a side that NFC changes", entry)
	}
	if unicode.Is(unicode.White_Space, side) {
		return fmt.Errorf("annotations.name_folds entry %q has a white space side", entry)
	}
	if unicode.Is(unicode.M, side) {
		return fmt.Errorf("annotations.name_folds entry %q has a combining mark side", entry)
	}
	return nil
}

// MustParseFolds is ParseFolds for a list that is known to be valid, for
// example one that config validation already accepted. An invalid list yields
// the empty fold set rather than a panic.
func MustParseFolds(list []string) Folds {
	folds, err := ParseFolds(list)
	if err != nil {
		return Folds{}
	}
	return folds
}

// Entries returns the validated entries in sorted order. It never returns nil,
// so a representation records `name_folds: []` when no fold is set.
func (f Folds) Entries() []string {
	out := make([]string, len(f.entries))
	copy(out, f.entries)
	return out
}

// Empty reports whether no fold is set.
func (f Folds) Empty() bool { return len(f.byFrom) == 0 }

// Equal reports whether two fold sets fold the same characters the same way.
func (f Folds) Equal(other Folds) bool {
	return EqualEntries(f.entries, other.entries)
}

// EqualEntries compares two recorded fold lists without regard to order.
func EqualEntries(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// PersonID is the person id normalization of SPEC §7.11.4, steps 1 to 6:
// NFC, default lower case, the configured folds, white space collapse, NFC
// again, and, when a fold is set, steps 3 to 5 again until the text does not
// change. The function is idempotent for every valid fold set, so a stored id
// and a requested value normalize to the same string.
func PersonID(name string, folds Folds) string {
	s := lowerCase(norm.NFC.String(name))
	s = foldPass(s, folds)
	if folds.Empty() {
		return s
	}
	// Step 6. The loop ends within len(runes)+2 passes (SPEC §7.11.4); the bound
	// below is a guard against a defect, not a rule.
	limit := utf8.RuneCountInString(s) + 3
	for pass := 0; pass < limit; pass++ {
		next := foldPass(s, folds)
		if next == s {
			break
		}
		s = next
	}
	return s
}

// foldPass is steps 3 to 5: one fold pass, white space collapse, NFC.
func foldPass(s string, folds Folds) string {
	if !folds.Empty() {
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			if to, ok := folds.byFrom[r]; ok {
				b.WriteRune(to)
				continue
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	return norm.NFC.String(collapseSpace(s))
}

// collapseSpace replaces each run of Unicode white space with one U+0020 and
// removes leading and trailing white space.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for _, r := range s {
		if unicode.Is(unicode.White_Space, r) {
			pending = b.Len() > 0
			continue
		}
		if pending {
			b.WriteByte(' ')
			pending = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lowerCase is the Unicode default lower case mapping (full case mapping, so
// U+0130 becomes U+0069 U+0307). A caser is stateful, so each call builds its
// own.
func lowerCase(s string) string {
	return cases.Lower(language.Und).String(s)
}
