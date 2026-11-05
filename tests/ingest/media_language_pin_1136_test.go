package tests

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestMediaLanguagePin_TranscriptIsConfigured is the #1136 regression at the
// ingest level: a corpus pinned with the media.stt.language spelling (an alias
// of media.language, SPEC §8.8) records the pinned language on the transcript
// with language_source=configured, even when the text reads as another
// language. Before #1136 the key was ignored and the transcript recorded the
// detected language.
func TestMediaLanguagePin_TranscriptIsConfigured(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	cfgPath := filepath.Join(testutil.TempDir(t), ".dir2mcp.yaml")
	writeFile(t, cfgPath, ""+
		"providers:\n"+
		"  whisper:\n"+
		"    kind: whisper\n"+
		"    base_url: http://127.0.0.1:9\n"+
		"    stt_model: large-v3\n"+
		"stt_provider: whisper\n"+
		"media:\n"+
		"  stt:\n"+
		"    language: uk\n")
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.LanguageDetectionEnabled = true

	st := newRealStore(t)
	svc := mustNewIngestService(t, cfg, st)
	// The English text would be detected as en: the pin must win (§8.8).
	svc.SetTranscriber(&fakeTranscriber{text: englishTranscript})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(transcriptMetaFor(t, st, "talk.mp3")), &p); err != nil {
		t.Fatalf("meta not json: %v", err)
	}
	if p["language"] != "uk" {
		t.Errorf("language = %v, want uk (the media.stt.language pin)", p["language"])
	}
	if p["language_source"] != "configured" {
		t.Errorf("language_source = %v, want configured", p["language_source"])
	}
}
