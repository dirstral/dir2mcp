package tests

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// syncBuffer is a goroutine-safe io.Writer used to capture Service log output
// (ingest may log from worker goroutines).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestMediaChunkCap_PDFTruncatedAndWarned pins #408 part 1: a document whose
// media-chunk fan-out exceeds the per-document cap is truncated to the cap (not
// crashed, not silently dropped) and a "truncated" diagnostic is emitted.
func TestMediaChunkCap_PDFTruncatedAndWarned(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	const pages = 20
	pdf := makeTestPDF(t, pages)
	if err := os.WriteFile(filepath.Join(root, "big.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, "augment")
	cfg.STTProvider = "off"
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := mustNewIngestService(t, cfg, st)
	const chunkCap = 5
	svc.MaxMediaChunksPerDoc = chunkCap
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))

	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, "big.pdf"), RelPath: "big.pdf", SizeBytes: int64(len(pdf))}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument must not fail when the media cap is hit: %v", err)
	}

	tasks, err := st.NextPending(context.Background(), 100, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	var pdfChunks int
	for _, tk := range tasks {
		if tk.Modality == "pdf" {
			pdfChunks++
		}
	}
	if pdfChunks != chunkCap {
		t.Fatalf("pdf media chunks = %d, want capped at %d (of %d pages)", pdfChunks, chunkCap, pages)
	}
	out := logs.String()
	if !strings.Contains(out, "truncated at 5") || !strings.Contains(out, "#408") {
		t.Errorf("expected a truncation warning mentioning the cap; got logs:\n%s", out)
	}
}

// TestMediaChunkCap_AudioWindowsTruncated pins #408 part 1 for time-window media:
// a long recording windowed below the cap is truncated to the cap rather than
// fanning out into unbounded windows.
func TestMediaChunkCap_AudioWindowsTruncated(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "long.mp3"), []byte("MEDIADATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadMultimodalConfig(t, root, "augment")
	cfg.STTProvider = "off"
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := mustNewIngestService(t, cfg, st)
	const capChunks = 3
	svc.MaxMediaChunksPerDoc = capChunks
	// 120s default audio window => a 1-hour clip would window into 30 chunks
	// without the cap.
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return time.Hour, nil }
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))

	df := ingest.DiscoveredFile{AbsPath: filepath.Join(root, "long.mp3"), RelPath: "long.mp3", SizeBytes: 9}
	if err := svc.ProcessDocument(context.Background(), df, nil, false); err != nil {
		t.Fatalf("ProcessDocument must not fail when the media cap is hit: %v", err)
	}
	tasks, err := st.NextPending(context.Background(), 100, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	var audioChunks int
	for _, tk := range tasks {
		if tk.Modality == "audio" {
			audioChunks++
		}
	}
	if audioChunks != capChunks {
		t.Fatalf("audio media chunks = %d, want capped at %d", audioChunks, capChunks)
	}
	if !strings.Contains(logs.String(), "truncated at 3") {
		t.Errorf("expected a truncation warning; got logs:\n%s", logs.String())
	}
}

// runArchiveIngestCapped writes archiveName into a temp root, ingests it with the
// given archive caps and a capturing logger, and returns the store plus captured
// log output.
func runArchiveIngestCapped(t *testing.T, archiveName string, archiveData []byte, maxMembers int, maxTotalBytes int64) (*store.SQLiteStore, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, archiveName), archiveData, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.RootDir = root
	svc := mustNewIngestService(t, cfg, st)
	svc.ArchiveMaxMembers = maxMembers
	svc.ArchiveMaxTotalBytes = maxTotalBytes
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st, logs.String()
}

// countArchiveMembers returns how many ingested documents are members of the
// given archive (rel_path prefixed with "<archive>/").
func countArchiveMembers(t *testing.T, st *store.SQLiteStore, archiveName string) int {
	t.Helper()
	docs, _, err := st.ListFiles(context.Background(), "", "", 10000, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	prefix := archiveName + "/"
	n := 0
	for _, d := range docs {
		if strings.HasPrefix(d.RelPath, prefix) {
			n++
		}
	}
	return n
}

// TestArchiveCap_MemberCountStopsAndWarns pins #408 part 2: an archive with more
// members than the member-count cap is stopped at the cap and reported, rather
// than buffering every member into memory.
func TestArchiveCap_MemberCountStopsAndWarns(t *testing.T) {
	files := make(map[string]string, 12)
	for i := 0; i < 12; i++ {
		files[fmt.Sprintf("f%02d.txt", i)] = fmt.Sprintf("content of member %d", i)
	}
	data := buildZip(t, files)
	const maxMembers = 4
	st, logs := runArchiveIngestCapped(t, "many.zip", data, maxMembers, 0)

	if got := countArchiveMembers(t, st, "many.zip"); got != maxMembers {
		t.Fatalf("ingested archive members = %d, want capped at %d", got, maxMembers)
	}
	if !strings.Contains(logs, "member fan-out exceeded caps") || !strings.Contains(logs, "#408") {
		t.Errorf("expected a member-cap warning; got logs:\n%s", logs)
	}
}

// TestArchiveCap_AggregateBytesStops pins #408 part 2: extraction halts once the
// aggregate uncompressed size would exceed the cap (decompression-bomb guard).
func TestArchiveCap_AggregateBytesStops(t *testing.T) {
	const memberSize = 1000
	body := strings.Repeat("a", memberSize)
	files := make(map[string]string, 6)
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("f%02d.txt", i)] = body
	}
	data := buildZip(t, files)
	// Cap at 2500 bytes: members 1 and 2 fit (2000B); the 3rd would reach 3000B
	// and is refused => exactly 2 members ingested.
	st, logs := runArchiveIngestCapped(t, "bomb.zip", data, 0, 2500)

	if got := countArchiveMembers(t, st, "bomb.zip"); got != 2 {
		t.Fatalf("ingested archive members = %d, want capped at 2 by aggregate-size", got)
	}
	if !strings.Contains(logs, "member fan-out exceeded caps") {
		t.Errorf("expected an aggregate-size cap warning; got logs:\n%s", logs)
	}
}

// TestArchiveCap_SingleCompressedAggregateBytesStops pins #408: the aggregate
// -size cap must be honored on the bare single-compressed (gz/bz2) path too, not
// just zip/tar. A .gz whose decompressed payload exceeds maxTotalBytes is refused
// and flagged truncated, consistent with the container paths.
func TestArchiveCap_SingleCompressedAggregateBytesStops(t *testing.T) {
	const bodySize = 1000
	data := buildGzip(t, strings.Repeat("a", bodySize))
	// Cap well below the decompressed payload: the single member cannot fit, so
	// nothing is ingested and the truncation warning fires.
	st, logs := runArchiveIngestCapped(t, "big.txt.gz", data, 0, 500)

	if got := countArchiveMembers(t, st, "big.txt.gz"); got != 0 {
		t.Fatalf("ingested archive members = %d, want 0 (payload exceeds aggregate-size cap)", got)
	}
	if !strings.Contains(logs, "member fan-out exceeded caps") || !strings.Contains(logs, "#408") {
		t.Errorf("expected an aggregate-size cap warning for single-compressed archive; got logs:\n%s", logs)
	}
}

// TestArchiveCap_SingleCompressedUnderCapIngested confirms the size cap on the
// single-compressed path is non-intrusive: a .gz whose payload fits under
// maxTotalBytes ingests its one member with no truncation warning.
func TestArchiveCap_SingleCompressedUnderCapIngested(t *testing.T) {
	data := buildGzip(t, "small payload")
	st, logs := runArchiveIngestCapped(t, "ok.txt.gz", data, 0, 1<<20)

	if got := countArchiveMembers(t, st, "ok.txt.gz"); got != 1 {
		t.Fatalf("ingested archive members = %d, want 1", got)
	}
	if strings.Contains(logs, "member fan-out exceeded caps") {
		t.Errorf("did not expect a truncation warning under the cap; got logs:\n%s", logs)
	}
}

// TestArchiveCap_UnderCapNotTruncated confirms the caps are non-intrusive: an
// archive within both limits ingests all members with no truncation warning.
func TestArchiveCap_UnderCapNotTruncated(t *testing.T) {
	files := map[string]string{
		"a.txt": "alpha",
		"b.txt": "bravo",
		"c.txt": "charlie",
	}
	data := buildZip(t, files)
	st, logs := runArchiveIngestCapped(t, "small.zip", data, 100, 1<<20)

	if got := countArchiveMembers(t, st, "small.zip"); got != len(files) {
		t.Fatalf("ingested archive members = %d, want all %d", got, len(files))
	}
	if strings.Contains(logs, "member fan-out exceeded caps") {
		t.Errorf("did not expect a truncation warning under the cap; got logs:\n%s", logs)
	}
}
