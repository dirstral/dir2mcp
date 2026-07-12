package tests

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// capturingTranscriber records the filename and bytes it is handed so a test can
// assert that a video's EXTRACTED AUDIO — not the raw container — reaches STT
// (issue #495).
type capturingTranscriber struct {
	text       string
	gotRelPath string
	gotBytes   []byte
	calls      int
}

func (c *capturingTranscriber) Transcribe(_ context.Context, relPath string, data []byte) (string, error) {
	c.calls++
	c.gotRelPath = relPath
	c.gotBytes = append([]byte(nil), data...)
	return c.text, nil
}

// TestVideoTranscript_RoutedThroughSTT is the regression guard for issue #495: a
// .mp4 video classified as doc_type "video" must be transcribed by extracting its
// audio track and feeding it to the configured STT provider, exactly like an audio
// file. Before the fix, only audio extensions were routed to STT, so a video was
// ingested but silently produced no transcript.
//
// ffmpeg is stubbed via ExtractAudioTrackFunc so the routing is exercised without
// the binary; the real audio extraction is covered by avutil.
func TestVideoTranscript_RoutedThroughSTT(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-container-bytes")
	st := newRealStore(t)

	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	tr := &capturingTranscriber{text: "[00:00] hello from the video\n[00:03] second line"}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")

	extractedAudio := []byte("fake-extracted-aac-audio")
	svc.ExtractAudioTrackFunc = func(_ context.Context, path string) ([]byte, error) {
		// avutil is handed the staged video temp file, whose extension is preserved
		// so ffmpeg infers the source muxer.
		if !strings.HasSuffix(path, ".mp4") {
			t.Errorf("audio extraction handed %q, want a staged .mp4 path", path)
		}
		return extractedAudio, nil
	}

	f := ingest.DiscoveredFile{RelPath: "clip.mp4", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(clip.mp4): %v", err)
	}

	// Routed through STT exactly once, with the EXTRACTED AUDIO under an audio
	// filename — not the raw .mp4 container the provider would reject.
	if tr.calls != 1 {
		t.Fatalf("STT calls = %d, want 1 (a video's audio track must be transcribed, #495)", tr.calls)
	}
	if string(tr.gotBytes) != string(extractedAudio) {
		t.Errorf("STT received %q, want the extracted audio bytes %q (the raw container must not be sent)", tr.gotBytes, extractedAudio)
	}
	if filepath.Ext(tr.gotRelPath) != ".m4a" {
		t.Errorf("STT received filename %q, want an audio (.m4a) extension so the provider infers an audio MIME type", tr.gotRelPath)
	}

	meta := transcriptMetaFor(t, st, "clip.mp4")
	if strings.TrimSpace(meta) == "" {
		t.Fatal("video produced no transcript representation (issue #495 routing gap)")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("transcript meta is not valid json (%q): %v", meta, err)
	}
	if parsed["source"] != "stt" {
		t.Errorf("transcript source = %v, want stt", parsed["source"])
	}
	if parsed["provider"] != "whisper" {
		t.Errorf("transcript provider = %v, want whisper", parsed["provider"])
	}

	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "ok" {
		t.Errorf("clip.mp4 status = %q, want ok (a transcribed video is searchable)", doc.Status)
	}
}

// TestVideoTranscript_NoAudioTrack_DegradesGracefully asserts the honest
// degradation path (#495): a video with no audio stream has nothing to transcribe,
// so STT is not invoked and the run does not fail. With no transcript, no subtitle
// sidecar, and multimodal keyframe embedding off, the soundless video is still
// surfaced as a durable status="error" so it is not a silent no-op (#398).
func TestVideoTranscript_NoAudioTrack_DegradesGracefully(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "silent.mp4"), "fake-video-no-audio")
	st := newRealStore(t)

	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	tr := &capturingTranscriber{text: "unused"}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return nil, avutil.ErrNoAudioStream
	}

	f := ingest.DiscoveredFile{RelPath: "silent.mp4", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(silent.mp4): %v (a soundless video must not fail the run)", err)
	}

	if tr.calls != 0 {
		t.Errorf("STT calls = %d, want 0 (a video with no audio track has nothing to transcribe)", tr.calls)
	}
	if meta, err := st.RepresentationMetaByType(context.Background(), "silent.mp4", ingest.RepTypeTranscript); err == nil && strings.TrimSpace(meta) != "" {
		t.Errorf("soundless video produced a transcript rep (%q); want none", meta)
	}

	doc := documentByPath(t, st, "silent.mp4")
	if doc.Status != "error" {
		t.Errorf("soundless video status = %q, want error (an unsearchable video must be durably visible, #398)", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "no representation") {
		t.Errorf("error_message = %q, want it to explain the video produced no representation", doc.ErrorMessage)
	}
}
