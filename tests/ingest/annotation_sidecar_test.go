package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// annotationVTT is byte-for-byte the v0 output of the dirstral-annotator
// pilot component (annotator/, dirstral-spec design 0004 §3): recognizer
// statements as WebVTT cues with the `[sources: …; confidence …]` tail.
// Design 0004 v0 promises this indexes through the EXISTING subtitle-sidecar
// mechanism with zero core changes; this test pins that promise.
const annotationVTT = "WEBVTT\n" +
	"\n" +
	"00:42:10.000 --> 00:42:31.000\n" +
	"Pitch: Logan Webb to Freddie Freeman — fly out [sources: playbyplay+scorebug; confidence 0.98]\n" +
	"\n" +
	"01:03:05.500 --> 01:03:12.000\n" +
	"Pitch: Logan Webb to Mookie Betts — swinging strike [sources: playbyplay; confidence 0.98]\n"

func TestAnnotationSidecar_VTTConvention_IndexesAsTimeSpannedTranscript(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "games", "game7.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, "games", "game7.vtt"), annotationVTT)

	st := &fakeIngestStore{}
	svc := newSidecarService(t, root, t.TempDir(), st) // exploding STT: sidecar must win

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the annotation VTT to be ingested as a sidecar")
	}

	// One authored sidecar transcript rep (source=sidecar, no language: the
	// annotator emits an undifferentiated "game7.vtt").
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected one bare transcript representation, got %+v", st.reps)
	}
	assertSidecarMeta(t, st.reps[0].MetaJSON, "")

	// The annotation statements must be in the indexed chunk text — this is
	// what makes "find all pitches by player X" a plain search query.
	var all strings.Builder
	for _, c := range st.chunks {
		all.WriteString(c.Text)
		all.WriteString("\n")
	}
	text := all.String()
	for _, want := range []string{
		"Pitch: Logan Webb to Freddie Freeman",
		"Pitch: Logan Webb to Mookie Betts",
		"confidence 0.98",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("indexed chunk text must contain %q, got:\n%s", want, text)
		}
	}

	// Citations must be `time` spans covering the annotated range
	// (00:42:10.000 → 01:03:12.000) so editors can cut clips from hits.
	if len(st.spans) == 0 {
		t.Fatal("expected at least one persisted span")
	}
	minStart, maxEnd := 1<<62, 0
	for _, sp := range st.spans {
		if sp.Kind != "time" {
			t.Fatalf("annotation sidecar spans must be kind=time, got %+v", sp)
		}
		if sp.StartMS < minStart {
			minStart = sp.StartMS
		}
		if sp.EndMS > maxEnd {
			maxEnd = sp.EndMS
		}
	}
	if minStart != 2530000 || maxEnd != 3792000 {
		t.Fatalf("expected span coverage 2530000..3792000 ms, got %d..%d", minStart, maxEnd)
	}
}
