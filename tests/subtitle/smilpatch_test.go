package tests

import (
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// A transcoder SMIL in the shape the TV archive uses: renditions as
// <video src="mp4:…"> in a body/switch, written by the transcoder, not by us.
const transcoderSMIL = `<?xml version='1.0' encoding='utf-8'?>
<smil>
  <head />
  <body>
    <switch>
      <video src="mp4:abc_1080p.mp4" systemLanguage="rus" height="1080" name="1080p">
        <param name="videoBitrate" value="3000000" valuetype="data" />
      </video>
      <video src="mp4:abc_720p.mp4" systemLanguage="rus" height="720" name="720p" />
      <video src="mp4:abc_180p.mp4" systemLanguage="rus" height="180" name="180p" />
    </switch>
  </body>
</smil>
`

var wowzaParams = []subtitle.SMILParam{{Name: "isWowzaCaptionStream", Value: "true"}}

func managedABC(src string) bool {
	switch src {
	case "abc.ru.vtt", "abc.en.vtt", "abc.ttml", "abc_1080p.ttml", "abc_720p.ttml":
		return true
	}
	return false
}

// TestInspectSMIL_TopRenditionAndValidation pins the top-rendition choice (the
// greatest height, "mp4:" stripped) and the three refusal reasons.
func TestInspectSMIL_TopRenditionAndValidation(t *testing.T) {
	info, err := subtitle.InspectSMIL([]byte(transcoderSMIL))
	if err != nil || info.TopVideoSrc != "abc_1080p.mp4" {
		t.Fatalf("top rendition = %q, err %v; want abc_1080p.mp4", info.TopVideoSrc, err)
	}
	if got := subtitle.SMILSrcStem("mp4:dir/abc_1080p.mp4"); got != "abc_1080p" {
		t.Fatalf("SMILSrcStem = %q", got)
	}
	cases := map[string]error{
		"<smil><body><switch>":                                             subtitle.ErrSMILUnparseable,
		"<smil><body><par/></body></smil>":                                 subtitle.ErrSMILNoSwitch,
		"<smil><body><switch><textstream src='x'/></switch></body></smil>": subtitle.ErrSMILNoVideo,
	}
	for doc, want := range cases {
		if _, err := subtitle.InspectSMIL([]byte(doc)); !errors.Is(err, want) {
			t.Fatalf("%q: err %v, want %v", doc, err, want)
		}
	}
}

// TestPatchSMIL_AddsOnlyCaptionLinesAndKeepsTheRestByteForByte pins the core
// contract: the patched SMIL is the original plus the new <textstream> lines,
// with every other byte unchanged.
func TestPatchSMIL_AddsOnlyCaptionLinesAndKeepsTheRestByteForByte(t *testing.T) {
	want := []subtitle.TextStream{{Src: "abc_1080p.ttml", Lang: "rus,eng", Params: wowzaParams}}
	out, changed, err := subtitle.PatchSMILTextStreams([]byte(transcoderSMIL), managedABC, want)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	added := `      <textstream src="abc_1080p.ttml" system-language="rus,eng">
        <param name="isWowzaCaptionStream" value="true" valuetype="data" />
      </textstream>
`
	expected := strings.Replace(transcoderSMIL, "    </switch>\n", added+"    </switch>\n", 1)
	if string(out) != expected {
		t.Fatalf("patched SMIL differs:\n--- got ---\n%s\n--- want ---\n%s", out, expected)
	}
	if _, err := subtitle.InspectSMIL(out); err != nil {
		t.Fatalf("patched SMIL must stay valid: %v", err)
	}
}

// TestPatchSMIL_NoChangeWhenAlreadyCorrect pins idempotency: a SMIL whose
// managed entries already equal the wanted set is returned untouched, so a
// correctly wired SMIL is never rewritten (and never gets a backup).
func TestPatchSMIL_NoChangeWhenAlreadyCorrect(t *testing.T) {
	want := []subtitle.TextStream{{Src: "abc_1080p.ttml", Lang: "rus,eng", Params: wowzaParams}}
	once, _, _ := subtitle.PatchSMILTextStreams([]byte(transcoderSMIL), managedABC, want)
	twice, changed, err := subtitle.PatchSMILTextStreams(once, managedABC, want)
	if err != nil || changed || string(twice) != string(once) {
		t.Fatalf("second patch must be a no-op: changed=%v err=%v", changed, err)
	}
	// The same entry written by livevtt (attribute order, quoting, mp4: prefix,
	// self-closing param) is recognised as equal too.
	livevtt := strings.Replace(transcoderSMIL, "    </switch>",
		"<textstream src=\"mp4:abc_1080p.ttml\" system-language=\"rus,eng\"><param valuetype=\"data\" value=\"true\" name=\"isWowzaCaptionStream\"/></textstream></switch>", 1)
	if _, changed, err := subtitle.PatchSMILTextStreams([]byte(livevtt), managedABC, want); err != nil || changed {
		t.Fatalf("a livevtt-wired SMIL must be left alone: changed=%v err=%v", changed, err)
	}
	// A param of another valuetype is not the entry the patch writes.
	text := strings.Replace(string(once), `value="true" valuetype="data"`, `value="true" valuetype="text"`, 1)
	fixed, changed, err := subtitle.PatchSMILTextStreams([]byte(text), managedABC, want)
	if err != nil || !changed || string(fixed) != string(once) {
		t.Fatalf("valuetype=\"text\" must be rewritten to data: changed=%v err=%v\n%s", changed, err, fixed)
	}
}

// TestPatchSMIL_ReplacesStaleManagedEntriesAndKeepsForeignOnes pins that a stale
// managed entry (the legacy group-named TTML, no param) is replaced, while a
// caption entry this caller does not manage is kept as it was.
func TestPatchSMIL_ReplacesStaleManagedEntriesAndKeepsForeignOnes(t *testing.T) {
	doc := strings.Replace(transcoderSMIL, "    </switch>",
		"      <textstream src=\"abc.ttml\" system-language=\"rus,eng\" />\n"+
			"      <textstream src=\"commentary.vtt\" system-language=\"eng\" />\n    </switch>", 1)
	want := []subtitle.TextStream{{Src: "abc_1080p.ttml", Lang: "rus,eng", Params: wowzaParams}}
	out, changed, err := subtitle.PatchSMILTextStreams([]byte(doc), managedABC, want)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if strings.Contains(s, `src="abc.ttml"`) {
		t.Fatalf("stale managed entry must be removed:\n%s", s)
	}
	if !strings.Contains(s, `<textstream src="commentary.vtt" system-language="eng" />`) {
		t.Fatalf("foreign entry must be kept as is:\n%s", s)
	}
	if strings.Count(s, "<textstream") != 2 || strings.Contains(s, "\n\n") {
		t.Fatalf("want exactly the foreign + the new entry and no blank lines:\n%s", s)
	}
}
