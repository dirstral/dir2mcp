package tests

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// Every dotted key the YAML parser dispatches through the string-scalar table
// must land on ITS OWN Config field. The table replaced a 15-case switch to get
// the setter under the complexity limit, and a table has one failure mode a
// switch does not: a row that points at the wrong field compiles, loads, and
// silently writes one setting into another. Nothing exercised those keys
// before, so this drives all fifteen through the real path — SaveFile, then
// LoadFile, then the parser's alias map, then the table — and compares each
// field to the distinct value it was given.
func TestStringFileScalars_EveryKeyLandsOnItsOwnField(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	// Distinct, valid-shaped values, so a swapped pointer cannot be masked by
	// two fields happening to hold the same string.
	cfg.IngestPDFMode = "extract"
	cfg.IngestImagesMode = "ocr"
	cfg.IngestAudioMode = "transcribe"
	cfg.IngestArchivesMode = "skip"
	cfg.IngestExtractor = "docling"
	cfg.IndexBackend = "memory"
	cfg.STTProvider = "whisper"
	cfg.STTMistralModel = "voxtral-mini-latest"
	cfg.STTElevenLabsModel = "scribe_v1"
	cfg.STTElevenLabsLanguageCode = "uk"
	cfg.MediaVariantsSelect = "first"
	cfg.MediaTranslateEngine = "chat"
	cfg.MediaSubtitlesSegmentation = "broadcast"
	cfg.MediaSubtitlesExpectScript = "cyrillic"
	cfg.MediaBatchManifest = "/tmp/repo/manifest.json"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	for _, f := range []struct{ key, got, want string }{
		{"ingest.pdf.mode", loaded.IngestPDFMode, cfg.IngestPDFMode},
		{"ingest.images.mode", loaded.IngestImagesMode, cfg.IngestImagesMode},
		{"ingest.audio.mode", loaded.IngestAudioMode, cfg.IngestAudioMode},
		{"ingest.archives.mode", loaded.IngestArchivesMode, cfg.IngestArchivesMode},
		{"ingest.extractor", loaded.IngestExtractor, cfg.IngestExtractor},
		{"index.backend", loaded.IndexBackend, cfg.IndexBackend},
		{"stt.provider", loaded.STTProvider, cfg.STTProvider},
		{"stt.mistral.model", loaded.STTMistralModel, cfg.STTMistralModel},
		{"stt.elevenlabs.model", loaded.STTElevenLabsModel, cfg.STTElevenLabsModel},
		{"stt.elevenlabs.language_code", loaded.STTElevenLabsLanguageCode, cfg.STTElevenLabsLanguageCode},
		{"media.variants.select", loaded.MediaVariantsSelect, cfg.MediaVariantsSelect},
		{"media.translate.engine", loaded.MediaTranslateEngine, cfg.MediaTranslateEngine},
		{"media.subtitles.segmentation", loaded.MediaSubtitlesSegmentation, cfg.MediaSubtitlesSegmentation},
		{"media.subtitles.expect_script", loaded.MediaSubtitlesExpectScript, cfg.MediaSubtitlesExpectScript},
		{"media.batch.manifest", loaded.MediaBatchManifest, cfg.MediaBatchManifest},
	} {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("%s landed as %q, want %q", f.key, f.got, f.want)
		}
	}
}
