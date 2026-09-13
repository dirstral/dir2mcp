package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// A partial transcript is otherwise permanent (#974).
//
// transcriptCacheKey is hash(media bytes) folded with the STT derivation
// identity (SPEC §8.6.7), and §8.6.13 requires a cache hit to restore the
// recorded coverage along with the text. An operator who repairs a down STT
// endpoint changes NEITHER key component, so an ordinary reindex restores the
// same partial transcript, reports the same shortfall, and never calls the
// provider. The recording can never be decoded again through the normal
// operating loop.
//
// These tests pin the escape and, just as carefully, pin that it stays scoped:
// clearing the whole cache would re-decode a corpus that is mostly fine, which
// on an archive is hours of GPU time and a provider bill.

// countingTranscriber records how many times it was actually asked to decode,
// which is the only thing these tests measure: a cache hit is invisible in the
// transcript text but shows up here as a call that did not happen.
type countingTranscriber struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (c *countingTranscriber) Transcribe(_ context.Context, _ string, _ []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.text == "" {
		return "[00:00] the original transcript", nil
	}
	return "[00:00] " + c.text, nil
}

func (c *countingTranscriber) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// redecodeService builds a Service over root with a counting transcriber and a
// state dir that persists across runs, so the transcript cache survives between
// ProcessDocument calls exactly as it does between reindexes.
func redecodeService(t *testing.T, root, stateDir string, st model.Store, tr model.Transcriber) *ingest.Service {
	t.Helper()
	cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(tr)
	return svc
}

func processMedia(t *testing.T, svc *ingest.Service, relPath string) {
	t.Helper()
	f := ingest.DiscoveredFile{RelPath: relPath, SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", relPath, err)
	}
}

func TestRedecode_AnOrdinaryRunStillTrustsTheCache(t *testing.T) {
	root, stateDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.mp3"), "fake-audio")
	tr := &countingTranscriber{}
	st := newRealStore(t)

	svc := redecodeService(t, root, stateDir, st, tr)
	processMedia(t, svc, "a.mp3")
	if tr.count() != 1 {
		t.Fatalf("first run: transcriber calls = %d, want 1", tr.count())
	}

	// A second Service over the same state dir, as a second reindex would be.
	svc2 := redecodeService(t, root, stateDir, newRealStore(t), tr)
	processMedia(t, svc2, "a.mp3")
	if tr.count() != 1 {
		t.Errorf("an ordinary run re-decoded without being asked: calls = %d, want 1", tr.count())
	}
}

func TestRedecode_AMarkedPathReachesTheProviderAndRewritesTheCache(t *testing.T) {
	root, stateDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.mp3"), "fake-audio")
	tr := &countingTranscriber{}
	st := newRealStore(t)

	svc := redecodeService(t, root, stateDir, st, tr)
	processMedia(t, svc, "a.mp3")
	if tr.count() != 1 {
		t.Fatalf("first run: calls = %d, want 1", tr.count())
	}

	// The endpoint has been repaired. The media bytes and the provider/model are
	// unchanged, so nothing about the cache key moved: only the mark gets us back
	// to the provider.
	tr.text = "the repaired transcript"
	svc2 := redecodeService(t, root, stateDir, newRealStore(t), tr)
	svc2.RedecodeTranscripts([]string{"a.mp3"})
	processMedia(t, svc2, "a.mp3")
	if tr.count() != 2 {
		t.Fatalf("a marked path must reach the provider: calls = %d, want 2", tr.count())
	}

	// The entry is REWRITTEN, so a repair costs exactly one decode rather than
	// leaving the document permanently uncached.
	if got := readCachedTranscript(t, stateDir); !strings.Contains(got, "repaired") {
		t.Fatalf("the cache still holds the stale text: %q", got)
	}
	svc3 := redecodeService(t, root, stateDir, newRealStore(t), tr)
	processMedia(t, svc3, "a.mp3")
	if tr.count() != 2 {
		t.Errorf("the rewritten cache was not used: calls = %d, want 2", tr.count())
	}
}

func TestRedecode_OnlyTheMarkedPathsAreDecodedAgain(t *testing.T) {
	root, stateDir := t.TempDir(), t.TempDir()
	// Different bytes, so the two recordings land on different cache keys.
	writeFile(t, filepath.Join(root, "partial.mp3"), "fake-audio-one")
	writeFile(t, filepath.Join(root, "fine.mp3"), "fake-audio-two")
	tr := &countingTranscriber{}

	svc := redecodeService(t, root, stateDir, newRealStore(t), tr)
	processMedia(t, svc, "partial.mp3")
	processMedia(t, svc, "fine.mp3")
	if tr.count() != 2 {
		t.Fatalf("first run: calls = %d, want 2", tr.count())
	}

	svc2 := redecodeService(t, root, stateDir, newRealStore(t), tr)
	svc2.RedecodeTranscripts([]string{"partial.mp3"})
	processMedia(t, svc2, "partial.mp3")
	processMedia(t, svc2, "fine.mp3")
	if tr.count() != 3 {
		t.Errorf("calls = %d, want 3: only the marked recording may be decoded again", tr.count())
	}
}

func TestRedecode_AnEmptySetRestoresOrdinaryCaching(t *testing.T) {
	// A Service must not be left permanently bypassing its own cache.
	root, stateDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "a.mp3"), "fake-audio")
	tr := &countingTranscriber{}

	svc := redecodeService(t, root, stateDir, newRealStore(t), tr)
	processMedia(t, svc, "a.mp3")

	svc.RedecodeTranscripts([]string{"a.mp3"})
	processMedia(t, svc, "a.mp3")
	before := tr.count()

	svc.RedecodeTranscripts(nil)
	processMedia(t, svc, "a.mp3")
	if tr.count() != before {
		t.Errorf("calls = %d, want %d: an empty set must restore cache reads", tr.count(), before)
	}
}

// readCachedTranscript returns the one cached transcript under stateDir.
func readCachedTranscript(t *testing.T, stateDir string) string {
	t.Helper()
	cacheDir := filepath.Join(stateDir, "cache", "transcribe")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			raw, err := os.ReadFile(filepath.Join(cacheDir, e.Name()))
			if err != nil {
				t.Fatalf("read cache entry: %v", err)
			}
			return string(raw)
		}
	}
	t.Fatalf("no cached transcript under %s", cacheDir)
	return ""
}

// There is deliberately NO test here for a rel_path with surrounding
// whitespace. The corpus resolver trims both ends before it touches the
// filesystem (`resolve "trailing.mp3 "` lstats `trailing.mp3`), so a document
// whose rel_path differs from its trimmed form cannot be read, ingested or
// stored, and the case is unreachable. RedecodeTranscripts nevertheless stores
// what it is given verbatim: the set mirrors paths that came OUT of the store,
// and normalizing a key you did not produce is a coupling to someone else's
// rules rather than a safeguard.
