package tests

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.6 sampled media identity (`ingest.media_identity: sampled`).

const identityTestMediaBytes = 20 << 20 // larger than the three 4 MiB slices

// byteCountingFS wraps a CorpusFS and counts the bytes read through Open, per
// corpus-relative path, so a test can assert what was and was not read.
type byteCountingFS struct {
	corpusfs.CorpusFS
	mu    sync.Mutex
	bytes map[string]int64
}

func newByteCountingFS(root string) *byteCountingFS {
	return &byteCountingFS{CorpusFS: corpusfs.NewLocalFS(root), bytes: map[string]int64{}}
}

func (c *byteCountingFS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	rc, err := c.CorpusFS.Open(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return &countingReader{ReadSeekCloser: rc, add: func(n int) {
		c.mu.Lock()
		c.bytes[relPath] += int64(n)
		c.mu.Unlock()
	}}, nil
}

func (c *byteCountingFS) read(relPath string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes[relPath]
}

type countingReader struct {
	io.ReadSeekCloser
	add func(int)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	r.add(n)
	return n, err
}

// lenRecordingTranscriber records the size of the media it was handed, to prove
// the lazily loaded content is the whole file.
type lenRecordingTranscriber struct {
	mu   sync.Mutex
	lens []int
}

func (f *lenRecordingTranscriber) Transcribe(_ context.Context, _ string, data []byte) (string, error) {
	f.mu.Lock()
	f.lens = append(f.lens, len(data))
	f.mu.Unlock()
	return "[00:00] first line\n[00:03] second line", nil
}

func writeRandomFile(t *testing.T, path string, n int) {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, buf)
}

type identityRun struct {
	root  string
	st    *store.SQLiteStore
	fs    *byteCountingFS
	trans *lenRecordingTranscriber
}

// runIdentityCorpus indexes root under the given media identity, two-phase with
// a translator to "en" configured, through a byte-counting corpus filesystem.
func runIdentityCorpus(t *testing.T, root, identity string, force bool) identityRun {
	t.Helper()
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return runIdentityCorpusOn(t, root, stateDir, st, identity, force)
}

func runIdentityCorpusOn(t *testing.T, root, stateDir string, st *store.SQLiteStore, identity string, force bool) identityRun {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.MediaBatchTwoPhase = true
	cfg.IngestMediaIdentity = identity
	svc := mustNewIngestService(t, cfg, st)
	fs := newByteCountingFS(root)
	svc.SetCorpusFS(fs)
	tr := &lenRecordingTranscriber{}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
	run := svc.Run
	if force {
		run = svc.Reindex
	}
	if err := run(context.Background()); err != nil {
		t.Fatalf("Run(%s): %v", identity, err)
	}
	return identityRun{root: root, st: st, fs: fs, trans: tr}
}

// TestSampledIdentity_SubtitledVideoIsNeverReadInFull is the point of the
// feature: a large video whose transcript comes from a sidecar is read only for
// its three slices, across both two-phase passes, while the default identity
// reads it whole. The transcript is the sidecar's either way.
func TestSampledIdentity_SubtitledVideoIsNeverReadInFull(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{"full", "sampled"} {
		identity := identity
		t.Run(identity, func(t *testing.T) {
			t.Parallel()
			root := testutil.TempDir(t)
			writeRandomFile(t, filepath.Join(root, "media", "clip.mp4"), identityTestMediaBytes)
			mustWriteFile(t, filepath.Join(root, "media", "clip.ru.vtt"),
				[]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nрепортаж\n"))
			r := runIdentityCorpus(t, root, identity, false)

			read := r.fs.read("media/clip.mp4")
			switch identity {
			case "sampled":
				if read > 3*(4<<20) {
					t.Fatalf("sampled identity read %d bytes of a subtitled video; want at most the three 4 MiB slices", read)
				}
			case "full":
				if read < identityTestMediaBytes {
					t.Fatalf("control: full identity should read the whole video, read %d", read)
				}
			}
			if len(r.trans.lens) != 0 {
				t.Fatalf("a subtitled video must not reach STT, got %d calls", len(r.trans.lens))
			}
			doc, err := r.st.GetDocumentByPath(context.Background(), "media/clip.mp4")
			if err != nil || doc.ContentHash == "" || doc.Status != "ok" {
				t.Fatalf("document not indexed: %+v err=%v", doc, err)
			}
			reps, _ := r.st.TranscriptRepresentations(context.Background(), "media/clip.mp4")
			if len(reps) == 0 {
				t.Fatalf("the sidecar transcript must be indexed")
			}
		})
	}
}

// TestSampledIdentity_STTGetsTheWholeFile pins the lazy load: a large media file
// with no sidecar still reaches STT with every byte, read when STT needs it.
func TestSampledIdentity_STTGetsTheWholeFile(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeRandomFile(t, filepath.Join(root, "audio", "long.mp3"), identityTestMediaBytes)
	r := runIdentityCorpus(t, root, "sampled", false)
	if len(r.trans.lens) == 0 {
		t.Fatalf("an unsubtitled file must be transcribed")
	}
	for _, n := range r.trans.lens {
		if n != identityTestMediaBytes {
			t.Fatalf("STT got %d bytes, want the whole %d-byte file", n, identityTestMediaBytes)
		}
	}
	reps, _ := r.st.TranscriptRepresentations(context.Background(), "audio/long.mp3")
	if len(reps) == 0 {
		t.Fatalf("the STT transcript must be indexed")
	}
}

// TestSampledIdentity_ChangeDetection pins what the sampled identity sees: a
// byte changed inside a slice changes the content hash; a byte changed outside
// the slices with the size unchanged does not (the documented trade-off).
func TestSampledIdentity_ChangeDetection(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	path := filepath.Join(root, "media", "clip.mp4")
	writeRandomFile(t, path, identityTestMediaBytes)
	// A sidecar lets the document complete without STT (the bytes are random,
	// so ffmpeg could not extract audio from them).
	mustWriteFile(t, filepath.Join(root, "media", "clip.ru.vtt"),
		[]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nрепортаж\n"))
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash := func() string {
		runIdentityCorpusOn(t, root, stateDir, st, "sampled", true)
		doc, err := st.GetDocumentByPath(context.Background(), "media/clip.mp4")
		if err != nil {
			t.Fatal(err)
		}
		if doc.ContentHash == "" || doc.Status != "ok" {
			t.Fatalf("document not indexed: status=%q hash=%q", doc.Status, doc.ContentHash)
		}
		return doc.ContentHash
	}
	// Each flip also stamps a new mtime: an unchanged (size, mtime) is skipped
	// without hashing (§7.8), and this test is about what the hash itself sees.
	stamp := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	flip := func(off int64) {
		defer func() {
			stamp = stamp.Add(time.Hour)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}()
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		b := make([]byte, 1)
		if _, err := f.ReadAt(b, off); err != nil {
			t.Fatal(err)
		}
		b[0] ^= 0xff
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}

	h0 := hash()
	if h1 := hash(); h1 != h0 {
		t.Fatalf("sampled identity is not stable: %q then %q", h0, h1)
	}
	flip(identityTestMediaBytes / 2) // inside the middle slice
	h2 := hash()
	if h2 == h0 {
		t.Fatalf("a change inside a sampled slice must change the identity")
	}
	flip(6 << 20) // between the head and middle slices
	if h3 := hash(); h3 != h2 {
		t.Fatalf("a same-size change outside the slices is documented as invisible; identity changed %q -> %q", h2, h3)
	}
}

// TestSampledIdentity_SmallMediaKeepsFullIdentity pins that a file no larger
// than the three slices is hashed whole, exactly as under "full", so switching
// the setting does not change small media's identity.
func TestSampledIdentity_SmallMediaKeepsFullIdentity(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeRandomFile(t, filepath.Join(root, "audio", "short.mp3"), 1<<20)
	full := runIdentityCorpus(t, root, "full", false)
	sampled := runIdentityCorpus(t, root, "sampled", false)
	a, _ := full.st.GetDocumentByPath(context.Background(), "audio/short.mp3")
	b, _ := sampled.st.GetDocumentByPath(context.Background(), "audio/short.mp3")
	if a.ContentHash == "" || a.ContentHash != b.ContentHash {
		t.Fatalf("small media must keep its full identity: full=%q sampled=%q", a.ContentHash, b.ContentHash)
	}
	if want := ingest.ComputeContentHash(mustReadFile(t, filepath.Join(root, "audio", "short.mp3"))); a.ContentHash != want {
		t.Fatalf("full identity of a sidecar-less file is the sha256 of its bytes: got %q want %q", a.ContentHash, want)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTwoPhase_DerivationNeverOverwritesAnAuthoredSidecar pins SPEC §8.6.11
// equivalence for a subtitled video under Whisper translation. Single-pass never
// translates a sidecar-sourced transcript (the sidecar is authoritative, §8.6.4),
// so the two-phase derivation pass must not either. Before this was fixed it did:
// finding no STT transcript in the cache, it ran STT on the spot, translated the
// result, and wrote it under transcript-en, overwriting the authored English
// sidecar. In an archive pilot it attempted this on ~550 subtitled videos: 121
// overwrote the sidecar, 408 failed translation, 20 failed STT. (Audio is used
// here because a fake video's audio cannot be extracted, which would hide it.)
func TestTwoPhase_DerivationNeverOverwritesAnAuthoredSidecar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
	mustWriteFile(t, filepath.Join(root, "media", "clip.ru.vtt"),
		[]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nрепортаж\n"))
	mustWriteFile(t, filepath.Join(root, "media", "clip.en.vtt"),
		[]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nauthored English\n"))
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.MediaBatchTwoPhase = true
	svc := mustNewIngestService(t, cfg, st)
	svcTranscriber := &fakeTranscriber{text: "[00:00] машинный текст"}
	svc.SetTranscriber(svcTranscriber)
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetTranscriptLanguage("ru")
	whisperTranslate := &fakeTranscriber{text: "[00:00] machine English"}
	svc.SetTranslateTranscriber(whisperTranslate, "whisper", "large-v3", []string{"en"})
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stt := svcTranscriber.calls; stt != 0 {
		t.Fatalf("the derivation pass must not run STT on a sidecar-sourced document, got %d STT calls", stt)
	}
	if whisperTranslate.calls != 0 {
		t.Fatalf("the derivation pass must not translate a sidecar-sourced document, got %d translate calls", whisperTranslate.calls)
	}
	reps, err := st.TranscriptRepresentations(ctx, "media/clip.mp3")
	if err != nil {
		t.Fatal(err)
	}
	sawEN := false
	for _, rep := range reps {
		if subexport.RepLanguage(rep.MetaJSON) != "en" {
			continue
		}
		sawEN = true
		if !subexport.RepIsSidecar(rep.MetaJSON) {
			t.Fatalf("the English transcript must stay the authored sidecar, got %s", rep.MetaJSON)
		}
	}
	if !sawEN {
		t.Fatalf("the authored English sidecar must be indexed")
	}
}
