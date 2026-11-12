package tests

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.3: classification is "extension + MIME sniff + binary heuristics".
// Only .mp4 and .mov were video by extension, so an archive lost every .mpg,
// .flv, .avi, .mkv (binary_ignored) and every .ts transport stream (TypeScript
// "code" that failed on the raw-text path).

// mpegTSBytes returns n plain MPEG-TS packets: the sync byte 0x47 every 188
// bytes, followed by a PAT-like header with NUL bytes.
func mpegTSBytes(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		pkt := make([]byte, 188)
		pkt[0] = 0x47
		pkt[1] = 0x40
		pkt[3] = 0x10
		b.Write(pkt)
	}
	return b.Bytes()
}

// bdavBytes returns n BDAV (M2TS) packets: a 4-byte timestamp before each
// 188-byte packet, so the sync byte is at 4, 196, 388.
func bdavBytes(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.Write([]byte{0x0e, 0xbf, 0x46, 0x22})
		pkt := make([]byte, 188)
		pkt[0] = 0x47
		pkt[1] = 0x40
		pkt[3] = 0x10
		b.Write(pkt)
	}
	return b.Bytes()
}

// mpegPSBytes returns a buffer that starts with an MPEG program stream pack
// header (00 00 01 BA), as a camcorder .mod file does.
func mpegPSBytes() []byte {
	b := make([]byte, 600)
	copy(b, []byte{0x00, 0x00, 0x01, 0xBA, 0x44, 0x00, 0x04, 0x00})
	return b
}

const typeScriptSource = "export function add(a: number, b: number): number {\n\treturn a + b\n}\n"

// textWithSyncLetters is valid TypeScript-like text that has the letter "G"
// (0x47) at offsets 0, 188 and 376. Only the binary check keeps it code.
func textWithSyncLetters() []byte {
	b := bytes.Repeat([]byte("x"), 400)
	for _, off := range []int{0, 188, 376} {
		b[off] = 'G'
	}
	return b
}

func TestClassifyDocType_VideoContainers(t *testing.T) {
	for _, tc := range []struct {
		path, want string
	}{
		{"clip.mp4", "video"},
		{"clip.mov", "video"},
		{"clip.mkv", "video"},
		{"clip.m4v", "video"},
		{"clip.webm", "video"},
		{"clip.avi", "video"},
		{"clip.flv", "video"},
		{"clip.wmv", "video"},
		{"clip.mpg", "video"},
		{"clip.mpeg", "video"},
		{"clip.mxf", "video"},
		{"clip.vob", "video"},
		{"clip.3gp", "video"},
		{"clip.m2ts", "video"},
		{"archive/1998/TAPE_01.MPG", "video"},
		{"clip.MKV", "video"},
		// Shared extensions: the path alone gives the other format. The content
		// decides (SniffMediaDocType).
		{"app.ts", "code"},
		{"app.mts", "code"},
		{"clip.mod", "binary_ignored"},
		// A basename rule wins over the .mod extension.
		{"go.mod", "data"},
		{"sub/go.mod", "data"},
		// Audio keeps its own type.
		{"talk.mp3", "audio"},
		{"talk.opus", "audio"},
	} {
		if got := ingest.ClassifyDocType(tc.path); got != tc.want {
			t.Errorf("ClassifyDocType(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSniffMediaDocType(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		content    []byte
		want       string
	}{
		{"ts transport stream", "news.ts", mpegTSBytes(4), "video"},
		{"ts uppercase extension", "NEWS.TS", mpegTSBytes(4), "video"},
		{"ts TypeScript source", "app.ts", []byte(typeScriptSource), "code"},
		{"ts text with G at the sync offsets", "app.ts", textWithSyncLetters(), "code"},
		{"ts too short for three packets", "app.ts", mpegTSBytes(1), "code"},
		{"ts BDAV variant", "disc.ts", bdavBytes(4), "video"},
		{"mts transport stream", "cam.mts", mpegTSBytes(4), "video"},
		{"mts BDAV variant", "cam.mts", bdavBytes(4), "video"},
		{"mts TypeScript module", "lib.mts", []byte(typeScriptSource), "code"},
		{"mod program stream", "MOV001.MOD", mpegPSBytes(), "video"},
		{"mod tracker module", "song.mod", append([]byte("M.K."), make([]byte, 600)...), "binary_ignored"},
		{"go.mod is never sniffed", "go.mod", mpegPSBytes(), "data"},
		{"m2ts is video by extension", "disc.m2ts", []byte("anything"), "video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pathType := ingest.ClassifyDocType(tc.path)
			if got := ingest.SniffMediaDocType(pathType, tc.path, tc.content); got != tc.want {
				t.Errorf("SniffMediaDocType(%q, %s) = %q, want %q", pathType, tc.path, got, tc.want)
			}
			// SniffTextDocType is the content refinement for an in-memory file (an
			// archive member). It must reach the same media verdict.
			if tc.want == "video" {
				if got := ingest.SniffTextDocType(pathType, tc.path, tc.content); got != "video" {
					t.Errorf("SniffTextDocType(%q, %s) = %q, want video", pathType, tc.path, got)
				}
			}
		})
	}
}

// TestMediaContainerTablesAgree pins that each extension the classifier calls
// video has a container MIME type for clips and media embedding, and that a
// .mod clip is muxed under an extension ffmpeg knows.
func TestMediaContainerTablesAgree(t *testing.T) {
	for _, ext := range []string{".mp4", ".mov", ".mkv", ".m4v", ".webm", ".avi", ".flv", ".wmv", ".mpg", ".mpeg", ".mxf", ".vob", ".3gp", ".m2ts", ".ts", ".mts", ".mod"} {
		if _, ok := avutil.VideoContainerMIME(ext); !ok {
			t.Errorf("VideoContainerMIME(%q) has no type", ext)
		}
		if got := avutil.SegmentOutputExt(ext); got == ".mod" {
			t.Errorf("SegmentOutputExt(%q) = %q; ffmpeg has no muxer for .mod", ext, got)
		}
	}
	if got := avutil.SegmentOutputExt(".MOD"); got != ".mpg" {
		t.Errorf("SegmentOutputExt(.MOD) = %q, want .mpg", got)
	}
	if got := avutil.SegmentOutputExt(".mkv"); got != ".mkv" {
		t.Errorf("SegmentOutputExt(.mkv) = %q, want .mkv", got)
	}
}

// upgradeVideoPaths are the files the upgrade test expects as video.
var upgradeVideoPaths = []string{"tape.mpg", "MOV001.MOD", "news.ts"}

// newUpgradeScanService returns an incremental ingest service whose STT is the
// given transcriber and whose audio extraction is a stub, so a video is
// transcribed without ffmpeg.
func newUpgradeScanService(t *testing.T, cfg config.Config, st model.Store, tr *capturingTranscriber) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")
	svc.ExtractAudioTrackFunc = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("fake-extracted-aac-audio"), nil
	}
	return svc
}

// seedOldClassifierRow stores a row for rel as an older classifier left it:
// the same bytes, size and mtime, so only the classification differs.
func seedOldClassifierRow(t *testing.T, st *store.SQLiteStore, root, rel string, body []byte, docType, status, skipReason, errMsg string) {
	t.Helper()
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:      rel,
		DocType:      docType,
		Status:       status,
		SkipReason:   skipReason,
		ErrorMessage: errMsg,
		ContentHash:  ingest.ComputeContentHash(body),
		SizeBytes:    int64(len(body)),
		MTimeUnix:    mtimeOf(t, filepath.Join(root, rel)),
	}); err != nil {
		t.Fatalf("seed %s: %v", rel, err)
	}
}

// assertIndexedAsVideo checks that each path is an ok video with a transcript.
func assertIndexedAsVideo(t *testing.T, st *store.SQLiteStore, when string) {
	t.Helper()
	for _, rel := range upgradeVideoPaths {
		doc := mustGetDoc(t, st, rel)
		if doc.DocType != "video" || doc.Status != "ok" {
			t.Errorf("%s %s: type=%q status=%q error=%q, want video/ok", rel, when, doc.DocType, doc.Status, doc.ErrorMessage)
		}
		if strings.TrimSpace(transcriptMetaFor(t, st, rel)) == "" {
			t.Errorf("%s %s has no transcript: it was relabelled, not processed as video", rel, when)
		}
	}
}

// TestMediaContainerUpgrade_IncrementalRunReclassifiesVideo is the upgrade
// path. A corpus indexed by an older binary holds a .mpg and a .mod as
// binary_ignored skips and a .ts transport stream as code with status error.
// The bytes, size and mtime are unchanged. A plain incremental run (a daemon
// restart, no reindex) must reclassify each as video and transcribe it. A
// TypeScript .ts stays code. A second incremental run then takes the cheap
// media fast path: nothing is transcribed again.
func TestMediaContainerUpgrade_IncrementalRunReclassifiesVideo(t *testing.T) {
	ctx := context.Background()
	root := testutil.TempDir(t)
	files := map[string][]byte{
		"tape.mpg": mpegPSBytes(),
		// Different bytes from tape.mpg, so content dedup cannot share one transcript.
		"MOV001.MOD": append(mpegPSBytes(), 0x01),
		"news.ts":    mpegTSBytes(4),
		"src/app.ts": []byte(typeScriptSource),
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(root, rel), string(body))
	}
	st := newRealStore(t)
	seedOldClassifierRow(t, st, root, "tape.mpg", files["tape.mpg"], "binary_ignored", "skipped", model.SkipReasonBinaryIgnored, "")
	seedOldClassifierRow(t, st, root, "MOV001.MOD", files["MOV001.MOD"], "binary_ignored", "skipped", model.SkipReasonBinaryIgnored, "")
	seedOldClassifierRow(t, st, root, "news.ts", files["news.ts"], "code", "error", "", "binary content on the raw-text path")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.STTProvider = "off"
	tr := &capturingTranscriber{text: "[00:00] archive footage\n[00:03] second line"}

	if err := newUpgradeScanService(t, cfg, st, tr).Run(ctx); err != nil {
		t.Fatalf("first incremental run: %v", err)
	}
	assertIndexedAsVideo(t, st, "after the upgrade run")
	if doc := mustGetDoc(t, st, "src/app.ts"); doc.DocType != "code" || doc.Status != "ok" {
		t.Errorf("src/app.ts: type=%q status=%q, want code/ok (TypeScript stays code)", doc.DocType, doc.Status)
	}
	if tr.calls != len(upgradeVideoPaths) {
		t.Fatalf("transcriber calls after the upgrade run = %d, want %d (one per video)", tr.calls, len(upgradeVideoPaths))
	}

	// A second run on unchanged files: each video keeps its stored type and
	// takes the (size, mtime) media fast path, so it is not processed again.
	if err := newUpgradeScanService(t, cfg, st, tr).Run(ctx); err != nil {
		t.Fatalf("second incremental run: %v", err)
	}
	if tr.calls != len(upgradeVideoPaths) {
		t.Errorf("transcriber calls after the second run = %d, want %d (an unchanged video must not be processed again)", tr.calls, len(upgradeVideoPaths))
	}
	assertIndexedAsVideo(t, st, "after the second run")
}

// TestMediaContainerFastPath_StoredVideoTsIsNotReRead pins the cheap fast paths
// for a .ts transport stream stored as video. The path alone says code, so
// without the stored type the scan would read the whole file on every run. A
// local file (no ETag) takes the (size, mtime) media skip; an S3 object takes
// the ETag skip. An older .mpg row stored as binary_ignored is re-read on the
// ETag path, because the classifier now calls its path video.
func TestMediaContainerFastPath_StoredVideoTsIsNotReRead(t *testing.T) {
	ts := string(mpegTSBytes(4))
	tsS3 := ts + "\x00"
	mpg := mpegPSBytes()
	const mtime = int64(1_700_000_000)
	fs := newFakeRemoteFS()
	fs.addWithMTime("local/news.ts", "", ts, mtime)
	fs.add("s3/news.ts", "etag-ts", tsS3)
	fs.add("s3/tape.mpg", "etag-mpg", string(mpg))

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID: 1, RelPath: "local/news.ts", DocType: "video", Status: "ok",
		SizeBytes: int64(len(ts)), MTimeUnix: mtime,
		ContentHash: ingest.ComputeContentHash([]byte(ts)),
	})
	st.seed(model.Document{
		DocID: 2, RelPath: "s3/news.ts", DocType: "video", Status: "ok",
		SizeBytes: int64(len(tsS3)), ETag: "etag-ts",
		ContentHash: ingest.ComputeContentHash([]byte(tsS3)),
	})
	st.seed(model.Document{
		DocID: 3, RelPath: "s3/tape.mpg", DocType: "binary_ignored", Status: "skipped",
		SkipReason: model.SkipReasonBinaryIgnored,
		SizeBytes:  int64(len(mpg)), ETag: "etag-mpg",
		ContentHash: ingest.ComputeContentHash(mpg),
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	for _, rel := range []string{"local/news.ts", "s3/news.ts"} {
		if got := fs.openCount(rel); got != 0 {
			t.Errorf("%s was opened %d time(s); a stored video .ts with an unchanged signal must take the fast path", rel, got)
		}
		if doc, ok := st.get(rel); !ok || doc.DocType != "video" {
			t.Errorf("%s doc_type = %q, want video", rel, doc.DocType)
		}
	}
	if got := fs.openCount("s3/tape.mpg"); got == 0 {
		t.Error("s3/tape.mpg took the ETag fast path; a stored binary_ignored row for a video extension must be re-read")
	}
	if doc, ok := st.get("s3/tape.mpg"); !ok || doc.DocType != "video" {
		t.Errorf("s3/tape.mpg doc_type = %q, want video", doc.DocType)
	}
}
