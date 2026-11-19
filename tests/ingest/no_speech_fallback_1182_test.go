package tests

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.6 + §8.2.4 (#1182): silence a ROUTE-FALLBACK decode produced is
// provisional like any fallback result. The no_speech row must still reach the
// fallback retry when it is due (route_retry_after elapsed, on_route_error:
// fail, or a new default profile), while silence the routed candidate itself
// produced is settled and never sent to STT again.

// silentFallbackServer is a whisper server on which every decode hears no
// speech. The identifier always reports Kazakh; kkBroken makes the Kazakh
// checkpoint refuse with a 400, so the default profile decodes the item.
type silentFallbackServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	models   []string
	kkBroken bool
}

func newSilentFallbackServer(t *testing.T, kkBroken bool) *silentFallbackServer {
	t.Helper()
	s := &silentFallbackServer{kkBroken: kkBroken}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := formModel(r)
		s.mu.Lock()
		s.models = append(s.models, m)
		broken := s.kkBroken
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case m == fbModelLID:
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "probe", "language": "kk", "language_probability": 0.9})
		case m == fbModelKK && broken:
			http.Error(w, "checkpoint not loaded", http.StatusBadRequest)
		default:
			_, _ = w.Write([]byte(`{"text":"","language":"kk","segments":[]}`))
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// decodes returns the decode requests (identifier probes excluded) made after
// the first n requests.
func (s *silentFallbackServer) decodes(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.models[n:] {
		if m != fbModelLID {
			out = append(out, m)
		}
	}
	return out
}

func (s *silentFallbackServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.models)
}

// silentFallbackCorpus is one audio file, a real store and a state dir shared
// by every service a test builds over it, as across daemon restarts.
type silentFallbackCorpus struct {
	root, state string
	st          *store.SQLiteStore
	stt         *silentFallbackServer
	file        ingest.DiscoveredFile
	now         time.Time
}

func newSilentFallbackCorpus(t *testing.T, kkBroken bool) *silentFallbackCorpus {
	t.Helper()
	c := &silentFallbackCorpus{root: testutil.TempDir(t), state: testutil.TempDir(t), st: newRealStore(t),
		stt: newSilentFallbackServer(t, kkBroken), now: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	writeFile(t, filepath.Join(c.root, "talk.mp3"), "fake-audio")
	c.file = ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	return c
}

// service builds a fresh ingest service over the corpus under onRouteError.
func (c *silentFallbackCorpus) service(t *testing.T, onRouteError string) *ingest.Service {
	t.Helper()
	yaml := "" +
		"stt_provider: whisper\n" +
		"providers:\n" +
		"  whisper:\n    kind: whisper\n    base_url: " + c.stt.srv.URL + "\n    stt_model: " + fbModelDefault + "\n" +
		"  ckpt-kk:\n    kind: whisper\n    base_url: " + c.stt.srv.URL + "\n    stt_model: " + fbModelKK + "\n" +
		"  lid:\n    kind: whisper\n    base_url: " + c.stt.srv.URL + "\n    stt_model: " + fbModelLID + "\n" +
		"media:\n  stt:\n    language_identifier: lid\n    on_route_error: " + onRouteError + "\n" +
		"    language_providers:\n      kk: ckpt-kk\n"
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.RootDir, cfg.StateDir = c.root, c.state
	cfg.QualityGatesEnabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	svc := mustNewIngestService(t, cfg, c.st)
	svc.SetLogger(log.New(&syncBuffer{}, "", 0))
	svc.SetClockForTest(func() time.Time { return c.now })
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 10 * time.Second, nil }
	svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) { return []byte("slice"), nil }
	return svc
}

func (c *silentFallbackCorpus) scan(t *testing.T, svc *ingest.Service) []string {
	t.Helper()
	before := c.stt.count()
	if err := svc.ProcessDocument(context.Background(), c.file, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	return c.stt.decodes(before)
}

func assertDecodedKK(t *testing.T, when string, decodes []string) {
	t.Helper()
	for _, m := range decodes {
		if m == fbModelKK {
			return
		}
	}
	t.Errorf("%s: decodes = %v, want the Kazakh route tried again (a due route-fallback result, SPEC §8.2.4)", when, decodes)
}

// TestNoSpeech_FallbackSilenceRetriedWhenDue: silence from a fallback decode is
// a no_speech skip that records its fallback stamp. It stands until
// route_retry_after elapses, and is then decoded again on its route.
func TestNoSpeech_FallbackSilenceRetriedWhenDue(t *testing.T) {
	t.Parallel()
	c := newSilentFallbackCorpus(t, true)
	svc := c.service(t, "default")
	first := c.scan(t, svc)
	if len(first) == 0 || first[len(first)-1] != fbModelDefault {
		t.Fatalf("first scan decodes = %v, want the default profile to decode after the Kazakh route failed", first)
	}
	assertNoSpeechSkip(t, c.st, "talk.mp3")
	if doc := documentByPath(t, c.st, "talk.mp3"); !strings.Contains(doc.ErrorMessage, "route_fallback_at=") {
		t.Errorf("no_speech detail %q does not record the fallback stamp", doc.ErrorMessage)
	}

	c.now = c.now.Add(time.Hour)
	if got := c.scan(t, c.service(t, "default")); len(got) != 0 {
		t.Errorf("rescan before route_retry_after: decodes = %v, want none (the fallback is not due yet)", got)
	}
	assertNoSpeechSkip(t, c.st, "talk.mp3")

	c.now = c.now.Add(25 * time.Hour)
	assertDecodedKK(t, "rescan after route_retry_after", c.scan(t, c.service(t, "default")))
	assertNoSpeechSkip(t, c.st, "talk.mp3")
}

// TestNoSpeech_FallbackSilenceRetriedUnderFail: under on_route_error: fail a
// cached fallback result is stale at once, silence included.
func TestNoSpeech_FallbackSilenceRetriedUnderFail(t *testing.T) {
	t.Parallel()
	c := newSilentFallbackCorpus(t, true)
	c.scan(t, c.service(t, "default"))
	assertNoSpeechSkip(t, c.st, "talk.mp3")

	c.now = c.now.Add(time.Minute)
	assertDecodedKK(t, "rescan under on_route_error: fail", c.scan(t, c.service(t, "fail")))
}

// TestNoSpeech_RoutedSilenceIsNotRetried: silence the routed candidate itself
// produced is no fallback result, so no retry clock applies to it: a rescan a
// day later sends nothing to STT.
func TestNoSpeech_RoutedSilenceIsNotRetried(t *testing.T) {
	t.Parallel()
	c := newSilentFallbackCorpus(t, false)
	c.scan(t, c.service(t, "default"))
	assertNoSpeechSkip(t, c.st, "talk.mp3")
	if doc := documentByPath(t, c.st, "talk.mp3"); strings.Contains(doc.ErrorMessage, "route_fallback") {
		t.Errorf("no_speech detail %q records a fallback stamp for a routed decode", doc.ErrorMessage)
	}

	c.now = c.now.Add(48 * time.Hour)
	if got := c.scan(t, c.service(t, "default")); len(got) != 0 {
		t.Errorf("rescan of routed silence: decodes = %v, want none", got)
	}
	assertNoSpeechSkip(t, c.st, "talk.mp3")
}
