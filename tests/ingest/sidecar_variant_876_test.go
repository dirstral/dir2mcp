package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #876 (SPEC §8.6.4): a subtitle sidecar sitting next to a media file MUST
// be ingested as the transcript instead of running STT. A multi-rendition archive
// writes its sidecars on the BARE stem ("<sha>.ru.vtt") while every media file
// carries a rendition suffix ("<sha>_1080p.mp4"), so the exact-base rule bound
// nothing and every rendition went to STT. findSidecars now tries the §8.6.5
// normalized variant base as a second candidate, after the exact base.

// The normalized pass is OPT-IN: it runs only under `media.variants.group:
// true`, the §8.6.5 declaration that files sharing a normalized name are
// renditions of one work. Every test below that expects a normalized binding
// therefore sets the flag, and TestSidecar876_VariantsGroupOff_* pins the
// default-off behaviour for every corpus that never opted in.

// sha is a stand-in for the archive's sha1 episode stem.
const sha876 = "258bc7b4b3cfcdf684ea82d20328625d27aa8e07"

func vtt876(text string) string {
	return "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\n" + text + "\n"
}

// ttml876 is the archive's untagged manifest sidecar: it declares its language
// internally (xml:lang="ru"), so it collides with an authored "<sha>.ru.vtt".
const ttml876 = `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="ru">
  <body><div>
    <p begin="00:00:00.000" end="00:00:02.000">from the ttml</p>
  </div></body>
</tt>`

// groupedConfig is a corpus config with §8.6.5 rendition grouping enabled.
func groupedConfig(t *testing.T, root string) config.Config {
	t.Helper()
	return config.Config{RootDir: root, StateDir: t.TempDir(), MediaVariantsGroup: true}
}

// newGroupedSidecarService builds a service with rendition grouping enabled and
// an exploding transcriber, so a stray STT call fails the test.
func newGroupedSidecarService(t *testing.T, root, stateDir string, st model.Store) *ingest.Service {
	t.Helper()
	cfg := config.Config{RootDir: root, StateDir: stateDir, MediaVariantsGroup: true}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&explodingTranscriber{t: t})
	return svc
}

// TestSidecar876_BareStemSidecarBindsToRenditionSuffixedVideo is the archive
// case: "<sha>.ru.vtt" and "<sha>.en.vtt" bind to "<sha>_1080p.mp4" and produce
// per-language transcripts, with no STT call. It fails before the change (the
// bare stem is not a prefix of "<sha>_1080p").
func TestSidecar876_BareStemSidecarBindsToRenditionSuffixedVideo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sha876+"_1080p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, sha876+"_720p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, sha876+".ru.vtt"), vtt876("Вторая волна"))
	writeFile(t, filepath.Join(root, sha876+".en.vtt"), vtt876("The second wave"))

	st := &fakeIngestStore{}
	svc := newGroupedSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: sha876 + "_1080p.mp4", DocType: "video"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the bare-stem sidecars to bind to the rendition-suffixed video")
	}
	if len(st.reps) != 2 {
		t.Fatalf("expected 2 per-language transcript reps, got %d: %+v", len(st.reps), st.reps)
	}
	byType := map[string]model.Representation{}
	for _, rep := range st.reps {
		byType[rep.RepType] = rep
	}
	for _, lang := range []string{"en", "ru"} {
		rep, ok := byType[ingest.TranscriptRepType(lang)]
		if !ok {
			t.Fatalf("expected a %q rep, got %v", ingest.TranscriptRepType(lang), byType)
		}
		assertSidecarMeta(t, rep.MetaJSON, lang)
	}
}

// TestSidecar876_UntaggedTTMLYieldsToTaggedSidecars pins the .ttml decision: the
// archive writes "<sha>.ttml" beside the two VTTs. On the NORMALIZED base an
// untagged sidecar declares neither work nor language, and a TTML carries its own
// xml:lang, so binding it would append a second copy of the Russian cues. It is
// dropped while a tagged sidecar binds.
func TestSidecar876_UntaggedTTMLYieldsToTaggedSidecars(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sha876+"_1080p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, sha876+".ru.vtt"), vtt876("from the vtt"))
	writeFile(t, filepath.Join(root, sha876+".ttml"), ttml876)

	st := &fakeIngestStore{}
	svc := newGroupedSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: sha876 + "_1080p.mp4", DocType: "video"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.TranscriptRepType("ru") {
		t.Fatalf("expected exactly the ru transcript from the vtt, got %+v", st.reps)
	}
	for _, c := range st.chunks {
		if strings.Contains(c.Text, "from the ttml") {
			t.Fatalf("untagged ttml must not contribute cues beside a tagged sidecar: %q", c.Text)
		}
	}
}

// TestSidecar876_UntaggedTTMLBindsWhenItIsTheOnlyCandidate is the other half of
// the .ttml rule: with no tagged sidecar present the untagged file still binds,
// so a lone sidecar is never lost. It fails before the change.
func TestSidecar876_UntaggedTTMLBindsWhenItIsTheOnlyCandidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sha876+"_1080p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, sha876+".ttml"), ttml876)

	st := &fakeIngestStore{}
	svc := newGroupedSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: sha876 + "_1080p.mp4", DocType: "video"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the lone untagged ttml to bind on the normalized base")
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.TranscriptRepType("ru") {
		t.Fatalf("expected one ru transcript (the ttml declares xml:lang), got %+v", st.reps)
	}
}

// TestSidecar876_ExactBaseWinsOverNormalizedBase pins the precedence: when both
// "clip_720p.en.vtt" (exact base) and "clip.en.vtt" (normalized base) exist, the
// exact one supplies the "en" transcript and the normalized one is dropped, so
// the cues are never duplicated.
func TestSidecar876_ExactBaseWinsOverNormalizedBase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip_720p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, "clip_720p.en.vtt"), vtt876("exact base wins"))
	writeFile(t, filepath.Join(root, "clip.en.vtt"), vtt876("normalized base loses"))

	st := &fakeIngestStore{}
	svc := newGroupedSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "clip_720p.mp4", DocType: "video"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.TranscriptRepType("en") {
		t.Fatalf("expected exactly one en transcript, got %+v", st.reps)
	}
	var text strings.Builder
	for _, c := range st.chunks {
		text.WriteString(c.Text)
	}
	if !strings.Contains(text.String(), "exact base wins") {
		t.Fatalf("expected the exact-base cues, got %q", text.String())
	}
	if strings.Contains(text.String(), "normalized base loses") {
		t.Fatalf("normalized-base sidecar must not duplicate the exact-base language, got %q", text.String())
	}
}

// TestSidecar876_NormalizedBase_RejectsBogusTokens runs every §8.6.4 rejection
// guard through the NORMALIZED path: a fragment token ("HD"), a year token
// ("2024"), an extra dotted segment, and the media's own extension token all stay
// rejected, so a bogus file cannot bind a fake language or suppress real STT.
func TestSidecar876_NormalizedBase_RejectsBogusTokens(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A bitrate marker makes "song_128k.mp3" a rendition of "song.mp3", so the
	// normalized base is "song" and every sibling below is a candidate.
	writeFile(t, filepath.Join(root, "song_128k.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "song.HD.vtt"), vtt876("hd fragment"))
	writeFile(t, filepath.Join(root, "song.2024.vtt"), vtt876("year fragment"))
	writeFile(t, filepath.Join(root, "song.notes.en.vtt"), vtt876("extra dotted segment"))
	writeFile(t, filepath.Join(root, "song.mp3.vtt"), vtt876("media extension token"))

	st := &fakeIngestStore{}
	stt := &fakeTranscriber{text: "[00:00] from stt"}
	svc := mustNewIngestService(t, groupedConfig(t, root), st)
	svc.SetTranscriber(stt)

	f := ingest.DiscoveredFile{RelPath: "song_128k.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if stt.calls != 1 {
		t.Fatalf("expected STT to run once (no bogus token may bind on the normalized base), got %d call(s)", stt.calls)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected one bare STT transcript rep, got %+v", st.reps)
	}
	if strings.Contains(st.reps[0].MetaJSON, `"source":"sidecar"`) {
		t.Fatalf("no bogus sidecar may bind, got meta %q", st.reps[0].MetaJSON)
	}
}

// TestSidecar876_NormalizedBase_GenuineLanguageStillBinds is the positive
// control for the test above: with the same normalized base, a real language
// sidecar binds and STT is skipped.
func TestSidecar876_NormalizedBase_GenuineLanguageStillBinds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song_128k.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "song.HD.vtt"), vtt876("hd fragment"))
	writeFile(t, filepath.Join(root, "song.ru.vtt"), vtt876("настоящая дорожка"))

	st := &fakeIngestStore{}
	svc := newGroupedSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: "song_128k.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the ru sidecar to bind on the normalized base")
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.TranscriptRepType("ru") {
		t.Fatalf("expected exactly one ru transcript, got %+v", st.reps)
	}
	for _, bogus := range []string{`"language":"HD"`, `"language":"hd"`} {
		if strings.Contains(st.reps[0].MetaJSON, bogus) {
			t.Fatalf("fragment token must not bind as a language, meta=%s", st.reps[0].MetaJSON)
		}
	}
}

// TestSidecar876_AudioRenditionStillNeedsItsOwnSidecar documents a limitation
// this change does NOT remove. "<sha>_audio.mp3" carries no rendition marker, so
// its normalized base equals its exact base and the bare-stem sidecar still does
// not bind: the audio rendition stays its own document and goes to STT. Grouping
// an audio-only rendition with its video siblings is a separate decision (#876).
func TestSidecar876_AudioRenditionStillNeedsItsOwnSidecar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sha876+"_audio.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, sha876+".ru.vtt"), vtt876("Вторая волна"))

	st := &fakeIngestStore{}
	stt := &fakeTranscriber{text: "[00:00] from stt"}
	svc := mustNewIngestService(t, groupedConfig(t, root), st)
	svc.SetTranscriber(stt)

	f := ingest.DiscoveredFile{RelPath: sha876 + "_audio.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if stt.calls != 1 {
		t.Fatalf("expected STT to run once for the unmarked audio rendition, got %d call(s)", stt.calls)
	}
}

// TestSidecar876_VariantsGroupOff_BareStemSidecarDoesNotBind is the guard for
// every corpus that never opted in. With `media.variants.group` at its default
// (false) the operator has NOT declared that files sharing a normalized name are
// one work, so the normalized pass does not run: the bare-stem sidecar does not
// bind and the media still goes to STT, exactly as before #876. Without this
// gate a "song.en.vtt" that belongs to a DIFFERENT work would silently become
// the transcript of "song_128k.mp3".
func TestSidecar876_VariantsGroupOff_BareStemSidecarDoesNotBind(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, sha876+"_1080p.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, sha876+".ru.vtt"), vtt876("Вторая волна"))
	writeFile(t, filepath.Join(root, sha876+".ttml"), ttml876)

	st := &fakeIngestStore{}
	// The default config leaves MediaVariantsGroup false.
	svc := newSidecarService(t, root, t.TempDir(), st)

	doc := model.Document{DocID: 1, RelPath: sha876 + "_1080p.mp4", DocType: "video"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if ingested {
		t.Fatal("with media.variants.group off, no bare-stem sidecar may bind")
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no transcript rep, got %+v", st.reps)
	}
}

// TestSidecar876_ExactBaseIdenticalUnderBothFlagValues pins that the flag only
// ever ADDS the normalized pass. An exact-base sidecar binds the same way, with
// the same language and the same cues, whether grouping is off or on.
func TestSidecar876_ExactBaseIdenticalUnderBothFlagValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		grouped bool
	}{
		{name: "group_off", grouped: false},
		{name: "group_on", grouped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "clip_720p.mp4"), "fake-video")
			writeFile(t, filepath.Join(root, "clip_720p.en.vtt"), vtt876("exact base cues"))

			st := &fakeIngestStore{}
			svc := newSidecarService(t, root, t.TempDir(), st)
			if tc.grouped {
				svc = newGroupedSidecarService(t, root, t.TempDir(), st)
			}

			doc := model.Document{DocID: 1, RelPath: "clip_720p.mp4", DocType: "video"}
			ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
			if err != nil {
				t.Fatalf("IngestSidecarTranscripts: %v", err)
			}
			if !ingested {
				t.Fatal("an exact-base sidecar must bind under either flag value")
			}
			if len(st.reps) != 1 || st.reps[0].RepType != ingest.TranscriptRepType("en") {
				t.Fatalf("expected one en transcript, got %+v", st.reps)
			}
			assertSidecarMeta(t, st.reps[0].MetaJSON, "en")
			var text strings.Builder
			for _, c := range st.chunks {
				text.WriteString(c.Text)
			}
			if !strings.Contains(text.String(), "exact base cues") {
				t.Fatalf("expected the exact-base cues, got %q", text.String())
			}
		})
	}
}
