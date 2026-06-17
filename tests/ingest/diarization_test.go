package tests

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestChunkSubtitleCues_VoiceTags_StableSpeakerIDs verifies that WebVTT <v>
// voice tags become stable, deterministic per-transcript speaker ids on the
// time span (SPEC §8.6.8): ids are allocated in first-appearance order, the same
// name maps to the same id, and chunks never merge across a speaker boundary.
func TestChunkSubtitleCues_VoiceTags_StableSpeakerIDs(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{StartMS: 0, EndMS: 1000, Text: "hello", Speaker: "Alice"},
		{StartMS: 1000, EndMS: 2000, Text: "hi back", Speaker: "Bob"},
		{StartMS: 2000, EndMS: 3000, Text: "again", Speaker: "Alice"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	if len(segs) != 3 {
		t.Fatalf("expected 3 chunks (split on speaker boundary), got %d: %+v", len(segs), segs)
	}
	want := []struct {
		id, label, text string
	}{
		{"S1", "Alice", "hello"},
		{"S2", "Bob", "hi back"},
		{"S1", "Alice", "again"}, // same name -> same id
	}
	for i, w := range want {
		if segs[i].Span.Speaker != w.id || segs[i].Span.SpeakerLabel != w.label {
			t.Errorf("chunk[%d] speaker=%q label=%q, want id=%q label=%q",
				i, segs[i].Span.Speaker, segs[i].Span.SpeakerLabel, w.id, w.label)
		}
		if segs[i].Text != w.text {
			t.Errorf("chunk[%d] text=%q, want %q", i, segs[i].Text, w.text)
		}
	}
}

// TestChunkSubtitleCues_Deterministic confirms the same input yields identical
// speaker ids across repeated runs (stable across re-indexing).
func TestChunkSubtitleCues_Deterministic(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{StartMS: 0, EndMS: 1000, Text: "a", Speaker: "X"},
		{StartMS: 1000, EndMS: 2000, Text: "b", Speaker: "Y"},
	}
	first := ingest.ChunkSubtitleCues(cues)
	second := ingest.ChunkSubtitleCues(cues)
	if len(first) != len(second) {
		t.Fatalf("non-deterministic chunk count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Span.Speaker != second[i].Span.Speaker {
			t.Errorf("chunk[%d] speaker id changed across runs: %q vs %q",
				i, first[i].Span.Speaker, second[i].Span.Speaker)
		}
	}
	if first[0].Span.Speaker != "S1" || first[1].Span.Speaker != "S2" {
		t.Errorf("unexpected ids: %q, %q", first[0].Span.Speaker, first[1].Span.Speaker)
	}
}

// TestChunkSubtitleCues_NoVoiceTags_MetadataOnly verifies that, with no voice
// tags, chunk text and span bounds are byte-identical to a non-diarized
// transcript and no speaker is set (speaker is purely additive metadata).
func TestChunkSubtitleCues_NoVoiceTags_MetadataOnly(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{StartMS: 0, EndMS: 1000, Text: "alpha"},
		{StartMS: 1000, EndMS: 2500, Text: "beta"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	if len(segs) != 1 {
		t.Fatalf("expected 1 merged chunk, got %d", len(segs))
	}
	if segs[0].Text != "alpha\nbeta" {
		t.Errorf("text = %q, want %q", segs[0].Text, "alpha\nbeta")
	}
	if segs[0].Span.StartMS != 0 || segs[0].Span.EndMS != 2500 {
		t.Errorf("span = [%d,%d], want [0,2500]", segs[0].Span.StartMS, segs[0].Span.EndMS)
	}
	if segs[0].Span.Speaker != "" || segs[0].Span.SpeakerLabel != "" {
		t.Errorf("expected no speaker, got id=%q label=%q", segs[0].Span.Speaker, segs[0].Span.SpeakerLabel)
	}
}

// fakeDiarizer is a deterministic test diarizer that attributes every segment to
// a single configured speaker, so the derivation-identity gate can be exercised
// without a real diarization backend.
type fakeDiarizer struct {
	id    string
	label string
	calls int
}

func (d *fakeDiarizer) Diarize(_ context.Context, _ []byte, segs []ingest.SpeakerSegment) ([]ingest.SpeakerAttribution, error) {
	d.calls++
	out := make([]ingest.SpeakerAttribution, len(segs))
	for i := range segs {
		out[i] = ingest.SpeakerAttribution{ID: d.id, Label: d.label}
	}
	return out, nil
}

// TestDiarizeModelSwap_InvalidatesAndRederives confirms a change to the
// diarization provider/model invalidates an unchanged transcript and re-derives
// it (SPEC §8.6.7/§8.6.8): the diarize identity is part of the transcript
// derivation identity.
func TestDiarizeModelSwap_InvalidatesAndRederives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: 1234567890}

	// Scan 1: diarized with pyannote v1.
	tr1 := &fakeTranscriber{text: "[00:00] hello world"}
	svc1 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr1)
	svc1.SetDiarizer(&fakeDiarizer{id: "S1", label: "Speaker"}, "whisper", "pyannote-v1")
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if tr1.calls != 1 {
		t.Fatalf("scan1 STT calls = %d, want 1", tr1.calls)
	}

	// Scan 2: same bytes + same STT model, but swap the diarize model to v2.
	tr2 := &fakeTranscriber{text: "[00:00] hello world"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr2)
	svc2.SetDiarizer(&fakeDiarizer{id: "S1", label: "Speaker"}, "whisper", "pyannote-v2")
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 1 {
		t.Fatalf("scan2 STT calls = %d, want 1 (diarize model swap must re-derive unchanged media)", tr2.calls)
	}
	meta := transcriptMetaFor(t, st, "talk.mp3")
	if want := `"diarize_model":"pyannote-v2"`; !strings.Contains(meta, want) {
		t.Fatalf("transcript meta not refreshed to new diarize identity (%s): %q", want, meta)
	}
}

// TestDiarizeNoSwap_NoChurn asserts re-scanning with the SAME diarize identity
// and unchanged content does NOT re-transcribe.
func TestDiarizeNoSwap_NoChurn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: 1234567890}

	tr1 := &fakeTranscriber{text: "[00:00] hello"}
	svc1 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr1)
	svc1.SetDiarizer(&fakeDiarizer{id: "S1", label: "Speaker"}, "whisper", "pyannote-v1")
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}

	tr2 := &fakeTranscriber{text: "[00:00] hello"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr2)
	svc2.SetDiarizer(&fakeDiarizer{id: "S1", label: "Speaker"}, "whisper", "pyannote-v1")
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 0 {
		t.Fatalf("scan2 STT calls = %d, want 0 (identical diarize identity + content must not re-derive)", tr2.calls)
	}
}

// TestSidecar_VoiceTags_DiarizedMeta verifies a sidecar carrying <v> tags
// produces a diarized transcript representation: meta_json records diarized:true
// and the speakers set, WITHOUT a diarize_provider/model (sidecar-sourced
// attribution is not model-derived, SPEC §8.6.8/§8.6.7).
func TestSidecar_VoiceTags_DiarizedMeta(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "interview.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "interview.vtt"),
		"WEBVTT\n\n"+
			"00:00:00.000 --> 00:00:02.000\n<v Host>Welcome\n\n"+
			"00:00:02.000 --> 00:00:04.000\n<v Guest>Thanks for having me\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "interview.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested || len(st.reps) != 1 {
		t.Fatalf("expected one ingested transcript rep, got ingested=%v reps=%d", ingested, len(st.reps))
	}

	var meta struct {
		Source          string `json:"source"`
		Diarized        bool   `json:"diarized"`
		DiarizeProvider string `json:"diarize_provider"`
		DiarizeModel    string `json:"diarize_model"`
		Speakers        []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal([]byte(st.reps[0].MetaJSON), &meta); err != nil {
		t.Fatalf("meta json: %v (%q)", err, st.reps[0].MetaJSON)
	}
	if meta.Source != "sidecar" {
		t.Errorf("source = %q, want sidecar", meta.Source)
	}
	if !meta.Diarized {
		t.Error("expected diarized:true")
	}
	if meta.DiarizeProvider != "" || meta.DiarizeModel != "" {
		t.Errorf("sidecar diarization must record NO provider/model, got %q/%q",
			meta.DiarizeProvider, meta.DiarizeModel)
	}
	if len(meta.Speakers) != 2 || meta.Speakers[0].ID != "S1" || meta.Speakers[0].Label != "Host" ||
		meta.Speakers[1].ID != "S2" || meta.Speakers[1].Label != "Guest" {
		t.Errorf("unexpected speakers set: %+v", meta.Speakers)
	}

	// Each chunk's time span must carry its speaker (per-segment attribution).
	if len(st.spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(st.spans))
	}
	if st.spans[0].Speaker != "S1" || st.spans[1].Speaker != "S2" {
		t.Errorf("span speakers = %q,%q want S1,S2", st.spans[0].Speaker, st.spans[1].Speaker)
	}
}

// TestSidecar_NoVoiceTags_NotDiarized confirms a plain sidecar (no <v> tags) is
// NOT marked diarized, so meta_json is unchanged from before diarization existed.
func TestSidecar_NoVoiceTags_NotDiarized(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plain.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "plain.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\njust a caption\n")

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "plain.mp3", DocType: "audio"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.reps) != 1 {
		t.Fatalf("expected one rep, got %d", len(st.reps))
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(st.reps[0].MetaJSON), &meta); err != nil {
		t.Fatalf("meta json: %v", err)
	}
	if _, ok := meta["diarized"]; ok {
		t.Errorf("plain sidecar must not set diarized, got meta=%v", meta)
	}
	if _, ok := meta["speakers"]; ok {
		t.Errorf("plain sidecar must not set speakers, got meta=%v", meta)
	}
	if len(st.spans) != 1 || st.spans[0].Speaker != "" {
		t.Errorf("expected one un-attributed span, got %+v", st.spans)
	}
}
