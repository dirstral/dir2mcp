package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// The record shape of SPEC §7.11.2 and the validation rules of §7.11.1. All
// data in this file is made up.

func parseFile(t *testing.T, body, keyKind string) (editorial.Batch, []editorial.LineError) {
	t.Helper()
	batch, problems, err := editorial.ParseFile(strings.NewReader(body), keyKind, "example-catalogue")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return batch, problems
}

// Conformance case 1, validation half: one invalid record makes the file
// invalid, and the problem names its line number.
func TestParseFile_PersonWithoutNameNamesTheLine(t *testing.T) {
	body := `{"stem": "abc", "people": [{"name": "Пётр Ёлкин"}]}

{"stem": "def", "people": [{"role": "инженер"}]}
`
	_, problems := parseFile(t, body, editorial.KeyStem)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", problems)
	}
	if problems[0].Line != 3 {
		t.Fatalf("problem line = %d, want 3 (blank lines still count)", problems[0].Line)
	}
	if !strings.Contains(problems[0].Reason, "people[0].name") {
		t.Fatalf("reason %q does not name the missing field", problems[0].Reason)
	}
}

// Conformance case 1, extra-field half.
func TestParseRecord_ExtraFieldsBecomeAttributesOrAreReported(t *testing.T) {
	body := `{"stem": "abc", "content_id": "C-1001", "match": {"score": 1.25, "asset_id": "A-7", "notes": {"a": 1}}, "people": [{"name": "Пётр Ёлкин", "x": 1}], "segments": [{"t": "00:00:04", "text": "Прогноз", "y": true}], "extra": null}
{"stem": "def", "people": [{"name": "Иван Сидоров", "x": "again"}]}
`
	batch, problems := parseFile(t, body, editorial.KeyStem)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	attrs := batch.Records[0].ProvenanceAttributes()
	want := map[string]string{"content_id": "C-1001", "match_asset_id": "A-7", "match_score": "1.25"}
	if !reflect.DeepEqual(attrs, want) {
		t.Fatalf("provenance attributes = %v, want %v", attrs, want)
	}
	wantIgnored := map[string]int{"people[].x": 2, "segments[].y": 1, "match.notes": 1, "extra": 1}
	if !reflect.DeepEqual(batch.Ignored, wantIgnored) {
		t.Fatalf("ignored = %v, want %v", batch.Ignored, wantIgnored)
	}
}

func TestParseRecord_InvalidRecords(t *testing.T) {
	cases := map[string]string{
		"reserved match_ prefix":   `{"stem": "a", "match_note": "x"}`,
		"reserved defined name":    `{"stem": "a", "role": "x"}`,
		"reserved dir2mcp: prefix": `{"stem": "a", "dir2mcp:x": "y"}`,
		"source differs":           `{"stem": "a", "source": "other"}`,
		"source differs by spaces": `{"stem": "a", "source": " example-catalogue"}`,
		"wrong type":               `{"stem": "a", "header": 7}`,
		"missing key":              `{"rel_path": "a.mp4"}`,
		"empty key":                `{"stem": "  "}`,
		"segment text empty":       `{"stem": "a", "segments": [{"t": 1, "text": " "}]}`,
		"t with comma":             `{"stem": "a", "segments": [{"t": "00:00,5", "text": "x"}]}`,
		"t negative":               `{"stem": "a", "segments": [{"t": -1, "text": "x"}]}`,
		"t minutes over 59":        `{"stem": "a", "segments": [{"t": "01:75:00", "text": "x"}]}`,
		"score not a number":       `{"stem": "a", "match": {"score": "1.25"}}`,
		"date not a date":          `{"stem": "a", "match": {"external_date": "01.01.2001"}}`,
		"not an object":            `["stem"]`,
		"two values":               `{"stem": "a"} {"stem": "b"}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := editorial.ParseRecord([]byte(line), editorial.KeyStem, "example-catalogue"); err == nil {
				t.Fatalf("ParseRecord(%s) accepted an invalid record", line)
			}
		})
	}
}

func TestParseRecord_ValidEdgeCases(t *testing.T) {
	cases := map[string]string{
		"no content":                 `{"stem": "a"}`,
		"source equal":               `{"stem": "a", "source": "example-catalogue"}`,
		"match dir2mcp member":       `{"stem": "a", "match": {"dir2mcp:x": "y"}}`,
		"other key field is defined": `{"stem": "a", "rel_path": "x/a.mp4"}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := editorial.ParseRecord([]byte(line), editorial.KeyStem, "example-catalogue"); err != nil {
				t.Fatalf("ParseRecord(%s): %v", line, err)
			}
		})
	}
}

func TestParseFile_DuplicateKeyMakesTheFileInvalid(t *testing.T) {
	_, problems := parseFile(t, "{\"stem\": \"a\"}\n{\"stem\": \"a\"}\n", editorial.KeyStem)
	if len(problems) != 1 || problems[0].Line != 2 {
		t.Fatalf("problems = %+v, want one on line 2", problems)
	}
}

func TestSegmentOffsets(t *testing.T) {
	cases := map[string]int64{
		`"00:00:04"`:   4000,
		`"01:00"`:      60000,
		`"02:30.5"`:    150500,
		`"7"`:          7000,
		`"1:02:03.25"`: 3723250,
		`12.3456`:      12346,
		`0`:            0,
	}
	for raw, want := range cases {
		line := `{"stem": "a", "segments": [{"t": ` + raw + `, "text": "x"}]}`
		rec, err := editorial.ParseRecord([]byte(line), editorial.KeyStem, "s")
		if err != nil {
			t.Fatalf("t=%s: %v", raw, err)
		}
		if got := rec.Segments[0].StartMS; got != want {
			t.Errorf("t=%s: start = %d ms, want %d", raw, got, want)
		}
	}
}

func TestScalarAttributeFormatting(t *testing.T) {
	line := `{"stem": "a", "n": 1.0, "big": 1e21, "f": false, "s": " as written "}`
	rec, err := editorial.ParseRecord([]byte(line), editorial.KeyStem, "s")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"n": "1", "big": "1000000000000000000000", "f": "false", "s": " as written "}
	if err == nil {
		exact, _ := editorial.ParseRecord([]byte(`{"stem": "a", "asset": 9007199254740993}`), editorial.KeyStem, "s")
		if exact.Extra["asset"] != "9007199254740993" {
			t.Fatalf("an integer above 2^53 changed: %q", exact.Extra["asset"])
		}
	}
	if !reflect.DeepEqual(rec.Extra, want) {
		t.Fatalf("extra = %v, want %v", rec.Extra, want)
	}
}

func TestValidateSourceAndKey(t *testing.T) {
	if err := editorial.ValidateSource(""); err == nil {
		t.Error("an empty source was accepted")
	}
	if err := editorial.ValidateSource(strings.Repeat("я", 129)); err == nil {
		t.Error("a 129-character source was accepted")
	}
	if err := editorial.ValidateSource("a\tb"); err == nil {
		t.Error("a source with a control character was accepted")
	}
	if err := editorial.ValidateSource(" wire "); err == nil {
		t.Error("a source with surrounding white space was accepted")
	}
	if err := editorial.ValidateSource(strings.Repeat("я", 128)); err != nil {
		t.Errorf("a 128-character source was refused: %v", err)
	}
	if _, err := editorial.ParseKeyKind("name"); err == nil {
		t.Error("an unknown --key was accepted")
	}
	if k, _ := editorial.ParseKeyKind(""); k != editorial.KeyStem {
		t.Errorf("default --key = %q, want stem", k)
	}
}
