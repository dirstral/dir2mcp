package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestParseVTT_VoiceTag_PopulatesSpeaker verifies the WebVTT <v Name> voice tag
// is parsed into Cue.Speaker (SPEC §8.6.8) while being stripped from the indexed
// cue text.
func TestParseVTT_VoiceTag_PopulatesSpeaker(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:02.000\n<v Roger Bingham>Hello there\n\n" +
		"00:00:02.000 --> 00:00:04.000\n<v Neil deGrasse Tyson>Good evening\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(cues))
	}
	if cues[0].Speaker != "Roger Bingham" {
		t.Errorf("cue[0].Speaker = %q, want %q", cues[0].Speaker, "Roger Bingham")
	}
	if cues[1].Speaker != "Neil deGrasse Tyson" {
		t.Errorf("cue[1].Speaker = %q, want %q", cues[1].Speaker, "Neil deGrasse Tyson")
	}
	// The voice tag MUST be stripped from indexed text (metadata only).
	if strings.Contains(cues[0].Text, "<v") || cues[0].Text != "Hello there" {
		t.Errorf("cue[0].Text = %q, want %q (voice tag must be stripped)", cues[0].Text, "Hello there")
	}
}

// TestParseVTT_VoiceTagWithClasses parses <v.class Name> (voice span with CSS
// classes), keeping only the name.
func TestParseVTT_VoiceTagWithClasses(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n<v.loud.first Bob>Shouting\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 1 || cues[0].Speaker != "Bob" {
		t.Fatalf("expected speaker Bob, got %+v", cues)
	}
}

// TestParseVTT_NoVoiceTag_EmptySpeaker confirms a transcript without voice tags
// carries no speaker, so behaviour is unchanged from before diarization.
func TestParseVTT_NoVoiceTag_EmptySpeaker(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nPlain caption\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 1 || cues[0].Speaker != "" {
		t.Fatalf("expected empty speaker, got %+v", cues)
	}
}

// TestRenderVTT_EmitsVoiceTag confirms a cue with a speaker re-exports the <v>
// voice tag (SPEC §8.6.3), and a cue without one renders unchanged.
func TestRenderVTT_EmitsVoiceTag(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{StartMS: 0, EndMS: 1000, Text: "Hi", Speaker: "Alice"},
		{StartMS: 1000, EndMS: 2000, Text: "Bye"},
	}
	out := subtitle.RenderVTT(cues)
	if !strings.Contains(out, "<v Alice>Hi") {
		t.Errorf("expected voice tag for Alice, got:\n%s", out)
	}
	// The speaker-less cue must not gain a voice tag.
	if strings.Contains(out, "<v >") || strings.Contains(out, "<v>Bye") {
		t.Errorf("speaker-less cue should not have a voice tag, got:\n%s", out)
	}
}
