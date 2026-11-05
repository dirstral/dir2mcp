package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.14 SMIL caption patch, end to end through ingest, in the shape the
// TV archive uses: renditions <hash>_<N>p.mp4, sidecars on the group stem, and a
// transcoder-written <hash>.smil.

const archiveSMIL = `<?xml version='1.0' encoding='utf-8'?>
<smil>
  <head />
  <body>
    <switch>
      <video src="mp4:abc_1080p.mp4" systemLanguage="rus" height="1080" name="1080p" />
      <video src="mp4:abc_720p.mp4" systemLanguage="rus" height="720" name="720p" />
    </switch>
  </body>
</smil>
`

const wiredTextStream = `      <textstream src="abc_1080p.ttml" system-language="rus,eng">
        <param name="isWowzaCaptionStream" value="true" valuetype="data" />
      </textstream>
`

func smilCorpus(t *testing.T, smil string) string {
	t.Helper()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "v", "abc_1080p.mp4"), []byte("fake-1080p-rendition"))
	mustWriteFile(t, filepath.Join(root, "v", "abc_720p.mp4"), []byte("fake-720p"))
	mustWriteFile(t, filepath.Join(root, "v", "abc.ru.vtt"), []byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nрепортаж\n"))
	mustWriteFile(t, filepath.Join(root, "v", "abc.en.vtt"), []byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nreport\n"))
	if smil != "" {
		mustWriteFile(t, filepath.Join(root, "v", "abc.smil"), []byte(smil))
	}
	return root
}

// runSMILCorpus indexes root with the archive's write-back + SMIL settings,
// through a fresh service over st (a fresh store when st is nil).
func runSMILCorpus(t *testing.T, root string, st *store.SQLiteStore, stateDir string) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	if st == nil {
		st = store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
		if err := st.Init(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.MediaVariantsGroup = true
	cfg.MediaVariantsSelect = "best"
	cfg.MediaSubtitlesTTMLEnabled = true
	cfg.MediaSubtitlesEmitEnabled = true
	cfg.MediaSubtitlesEmitFormats = []string{"vtt", "ttml"}
	cfg.MediaSubtitlesEmitSMIL = "ttml"
	cfg.MediaSubtitlesEmitSMILParams = []string{"isWowzaCaptionStream=true"}
	cfg.MediaSubtitlesEmitSMILLanguageCodes = "iso639_2"
	cfg.MediaSubtitlesEmitTTMLName = "top_rendition"
	cfg.MediaSubtitlesTTMLSegmentation = "broadcast"
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] машинный текст"})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

func smilBackups(t *testing.T, root string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root, "v", "abc.smil.bak.*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSMILPatch_WiresTheTopRenditionTTMLOnceWithABackup pins the livevtt
// behavior: the TTML is named after the SMIL's top rendition, the SMIL gains one
// caption entry (ISO 639-2 languages, the configured param) after a backup, and
// nothing else in it changes. A second scan changes nothing and backs up nothing.
func TestSMILPatch_WiresTheTopRenditionTTMLOnceWithABackup(t *testing.T) {
	t.Parallel()
	root := smilCorpus(t, archiveSMIL)
	stateDir := testutil.TempDir(t)
	st := runSMILCorpus(t, root, nil, stateDir)

	if !fileExists(filepath.Join(root, "v", "abc_1080p.ttml")) || fileExists(filepath.Join(root, "v", "abc.ttml")) {
		t.Fatalf("the TTML must be named after the top rendition (abc_1080p.ttml), not the group")
	}
	smilPath := filepath.Join(root, "v", "abc.smil")
	got := readFile(t, smilPath)
	want := strings.Replace(archiveSMIL, "    </switch>\n", wiredTextStream+"    </switch>\n", 1)
	if got != want {
		t.Fatalf("patched SMIL differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	backups := smilBackups(t, root)
	if len(backups) != 1 || readFile(t, backups[0]) != archiveSMIL {
		t.Fatalf("want exactly one backup holding the original SMIL, got %v", backups)
	}
	info1, _ := os.Stat(smilPath)

	runSMILCorpus(t, root, st, stateDir)
	if readFile(t, smilPath) != want {
		t.Fatalf("a second scan must leave the wired SMIL as it is")
	}
	if info2, _ := os.Stat(smilPath); !info2.ModTime().Equal(info1.ModTime()) {
		t.Fatalf("a second scan must not rewrite the SMIL")
	}
	if n := len(smilBackups(t, root)); n != 1 {
		t.Fatalf("a second scan must not back up again, backups=%d", n)
	}
}

// TestSMILPatch_LeavesALivevttWiredSMILAlone pins the cutover case: a video
// livevtt already captioned (its rendition-named TTML present and the SMIL
// entry in place) is not rewritten, backed up, or re-captioned.
func TestSMILPatch_LeavesALivevttWiredSMILAlone(t *testing.T) {
	t.Parallel()
	wired := strings.Replace(archiveSMIL, "    </switch>\n", wiredTextStream+"    </switch>\n", 1)
	root := smilCorpus(t, wired)
	ttml := `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en"><body><div xml:lang="rus"><p begin="00:00:00.000" end="00:00:02.000">livevtt</p></div></body></tt>
`
	mustWriteFile(t, filepath.Join(root, "v", "abc_1080p.ttml"), []byte(ttml))
	runSMILCorpus(t, root, nil, testutil.TempDir(t))

	if readFile(t, filepath.Join(root, "v", "abc.smil")) != wired {
		t.Fatalf("a correctly wired SMIL must be left byte-for-byte as it was")
	}
	if n := len(smilBackups(t, root)); n != 0 {
		t.Fatalf("no change means no backup, got %d", n)
	}
	if readFile(t, filepath.Join(root, "v", "abc_1080p.ttml")) != ttml {
		t.Fatalf("livevtt's TTML must never be overwritten")
	}
}

// TestSMILPatch_NeverCreatesASMIL pins that a missing SMIL is reported, not
// created, and the subtitles are still written.
func TestSMILPatch_NeverCreatesASMIL(t *testing.T) {
	t.Parallel()
	root := smilCorpus(t, "")
	runSMILCorpus(t, root, nil, testutil.TempDir(t))
	if fileExists(filepath.Join(root, "v", "abc.smil")) {
		t.Fatalf("a SMIL must never be created")
	}
	if !fileExists(filepath.Join(root, "v", "abc_1080p.ttml")) {
		t.Fatalf("the TTML must still be written (named after the media's own rendition)")
	}
}
