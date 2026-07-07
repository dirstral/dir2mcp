package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestAlignBilingualWithinTolerance pins that a secondary cue whose start is
// within the alignment tolerance of a primary cue is merged into that primary
// cue (same time region, two language runs), while a secondary cue outside
// tolerance is emitted as its own secondary-only cue rather than dropped
// (SPEC §8.6.10).
func TestAlignBilingualWithinTolerance(t *testing.T) {
	primary := []subtitle.Cue{
		{StartMS: 0, EndMS: 2000, Text: "Hello"},
		{StartMS: 5000, EndMS: 7000, Text: "World"},
	}
	secondary := []subtitle.Cue{
		// 200 ms after the first primary start: within 2500 ms -> merged.
		{StartMS: 200, EndMS: 2100, Text: "Bonjour"},
		// 9000 ms: no primary within 2500 ms -> own secondary-only cue.
		{StartMS: 9000, EndMS: 10000, Text: "Orphelin"},
	}

	got := subtitle.AlignBilingual(primary, secondary, "en", "fr", 2500)
	if len(got) != 3 {
		t.Fatalf("expected 3 cues (2 primary + 1 orphan), got %d: %+v", len(got), got)
	}

	// First cue carries both languages over the primary time region.
	c0 := got[0]
	if c0.StartMS != 0 || c0.EndMS != 2000 {
		t.Fatalf("merged cue must keep primary time region [0,2000], got [%d,%d]", c0.StartMS, c0.EndMS)
	}
	if c0.PrimaryText != "Hello" || c0.PrimaryLang != "en" {
		t.Fatalf("primary run wrong: %+v", c0)
	}
	if c0.SecondaryText != "Bonjour" || c0.SecondaryLang != "fr" {
		t.Fatalf("secondary run not aligned to same region: %+v", c0)
	}

	// Second primary has no secondary in tolerance.
	if got[1].SecondaryText != "" {
		t.Fatalf("second primary should have no secondary run: %+v", got[1])
	}

	// Orphan secondary emitted as its own cue, not dropped.
	last := got[2]
	if last.StartMS != 9000 || last.PrimaryText != "Orphelin" || last.PrimaryLang != "fr" {
		t.Fatalf("orphan secondary cue wrong: %+v", last)
	}
}

// TestAlignBilingualOverlapWins pins the issue #441 fix: pairing is overlap-first,
// so a secondary is merged into the primary whose TIME REGION it overlaps most —
// not merely the primary whose start is nearest. A greedy start-only matcher with
// a 2500 ms tolerance would mis-pair these; overlap-aware alignment does not.
func TestAlignBilingualOverlapWins(t *testing.T) {
	// Two adjacent primaries. The secondary starts 100 ms before P1 but sits
	// squarely inside P1's region; a start-distance matcher within tolerance could
	// steal it onto P0 (start distance 1900 ms < 2500 ms). Overlap must win.
	primary := []subtitle.Cue{
		{StartMS: 0, EndMS: 1800, Text: "P0"},
		{StartMS: 2000, EndMS: 3800, Text: "P1"},
	}
	secondary := []subtitle.Cue{
		{StartMS: 1900, EndMS: 3700, Text: "S1"},
	}
	got := subtitle.AlignBilingual(primary, secondary, "en", "fr", 2500)

	var p1 *subtitle.BilingualCue
	for i := range got {
		if got[i].PrimaryText == "P1" {
			p1 = &got[i]
		}
		if got[i].PrimaryText == "P0" && got[i].SecondaryText != "" {
			t.Fatalf("secondary mis-paired onto P0 (start-nearest): %+v", got[i])
		}
	}
	if p1 == nil {
		t.Fatalf("P1 cue missing: %+v", got)
	}
	if p1.SecondaryText != "S1" || p1.SecondaryLang != "fr" {
		t.Fatalf("secondary S1 should merge into P1 (max overlap), got %+v", *p1)
	}
}

// TestAlignBilingualLongCueNotStolen pins fix 3.2: a translation of a long cue is
// not stolen by a short neighbor that merely starts nearer. The secondary overlaps
// the long primary heavily; the short primary starts closer but shares no region.
func TestAlignBilingualLongCueNotStolen(t *testing.T) {
	primary := []subtitle.Cue{
		{StartMS: 1000, EndMS: 1200, Text: "SHORT"},
		{StartMS: 1300, EndMS: 6000, Text: "LONG"},
	}
	secondary := []subtitle.Cue{
		// Starts 500 ms after SHORT (nearest start) but overlaps LONG for ~4 s.
		{StartMS: 1500, EndMS: 5800, Text: "T"},
	}
	got := subtitle.AlignBilingual(primary, secondary, "en", "fr", 2500)
	for _, c := range got {
		if c.PrimaryText == "SHORT" && c.SecondaryText != "" {
			t.Fatalf("translation stolen by short neighbor: %+v", c)
		}
		if c.PrimaryText == "LONG" && c.SecondaryText != "T" {
			t.Fatalf("translation should merge into LONG (max overlap), got %+v", c)
		}
	}
}

// TestAlignBilingualDeterministic pins deterministic alignment: repeated runs
// over identical inputs produce identical cue slices.
func TestAlignBilingualDeterministic(t *testing.T) {
	primary := []subtitle.Cue{
		{StartMS: 1000, EndMS: 2000, Text: "A"},
		{StartMS: 1100, EndMS: 2100, Text: "B"},
	}
	secondary := []subtitle.Cue{
		{StartMS: 1050, EndMS: 2050, Text: "x"},
		{StartMS: 1150, EndMS: 2150, Text: "y"},
	}
	first := subtitle.RenderTTML(subtitle.AlignBilingual(primary, secondary, "en", "fr", 2500), "en")
	for i := 0; i < 5; i++ {
		again := subtitle.RenderTTML(subtitle.AlignBilingual(primary, secondary, "en", "fr", 2500), "en")
		if again != first {
			t.Fatalf("alignment not deterministic on run %d", i)
		}
	}
}

// TestRenderTTMLBilingual pins that bilingual TTML emits language-tagged spans
// over a single <p> time region, both traceable to the same cue.
func TestRenderTTMLBilingual(t *testing.T) {
	cues := []subtitle.BilingualCue{
		{
			StartMS: 0, EndMS: 1500,
			PrimaryLang: "en", PrimaryText: "Hello",
			SecondaryLang: "fr", SecondaryText: "Bonjour",
		},
	}
	out := subtitle.RenderTTML(cues, "en")
	for _, want := range []string{
		`<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en">`,
		`<p begin="00:00:00.000" end="00:00:01.500">`,
		`<span xml:lang="en">Hello</span>`,
		`<span xml:lang="fr">Bonjour</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("TTML missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderTTMLEscapesText pins XML escaping of cue text and newline -> <br/>.
func TestRenderTTMLEscapesText(t *testing.T) {
	cues := []subtitle.BilingualCue{
		{StartMS: 0, EndMS: 1000, PrimaryLang: "en", PrimaryText: "a < b & c\nline2"},
	}
	out := subtitle.RenderTTML(cues, "en")
	if !strings.Contains(out, "a &lt; b &amp; c<br/>line2") {
		t.Fatalf("expected escaped text with <br/>, got:\n%s", out)
	}
}

// TestRenderSMILMetadataAndFailOpen pins that SMIL carries probed codec/bitrate/
// dimensions when present, and that a zero MediaInfo (the fail-open case after a
// failed probe) still renders a valid audio-only SMIL with no width/height —
// callers omit SMIL entirely on probe failure, but a partial probe must not
// crash or emit bogus dimensions (SPEC §8.6.10).
func TestRenderSMILMetadataAndFailOpen(t *testing.T) {
	full := subtitle.RenderSMIL(subtitle.SMILInput{
		MediaSrc: "clip.mp4",
		Info: avutil.MediaInfo{
			Container: "mov,mp4", VideoCodec: "h264", AudioCodec: "aac",
			BitRateBPS: 800000, Width: 1280, Height: 720,
		},
		Subtitles: []subtitle.SMILSubtitleRef{{Src: "clip.ttml", Lang: "en"}},
	})
	for _, want := range []string{
		`name="videoCodec" content="h264"`,
		`name="bitrate" content="800000"`,
		`<video src="clip.mp4" width="1280" height="720"/>`,
		`<textstream src="clip.ttml" systemLanguage="en"/>`,
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("SMIL missing %q in:\n%s", want, full)
		}
	}

	// Partial/empty MediaInfo: no dimensions, falls back to <audio>, no bogus
	// width/height attributes.
	partial := subtitle.RenderSMIL(subtitle.SMILInput{
		MediaSrc:  "clip.mp4",
		Info:      avutil.MediaInfo{},
		Subtitles: []subtitle.SMILSubtitleRef{{Src: "clip.ttml", Lang: "en"}},
	})
	if strings.Contains(partial, "width=") || strings.Contains(partial, "<video") {
		t.Fatalf("empty MediaInfo must not emit video/dimensions, got:\n%s", partial)
	}
	if !strings.Contains(partial, `<audio src="clip.mp4"/>`) {
		t.Fatalf("empty MediaInfo should fall back to <audio>, got:\n%s", partial)
	}
}
