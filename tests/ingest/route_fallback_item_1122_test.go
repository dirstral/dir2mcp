package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
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

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): a route-fallback transcript is
// provisional. These tests cover the item scope. The fake whisper server below
// selects the checkpoint by the `model` form field, as a local whisper server
// does, and the Kazakh checkpoint can be broken and repaired between runs.

const (
	fbModelDefault = "base-model"
	fbModelKK      = "ckpt-kk"
	fbModelLID     = "lid-model"
	fbDefaultID    = "whisper|" + fbModelDefault
)

// fallbackSTTServer records every `model` it receives and answers per model.
// The identifier always hears Kazakh. kkBroken makes the Kazakh checkpoint
// fail with a 400 (not retried by the client); defaultBroken does the same to
// the default profile.
type fallbackSTTServer struct {
	srv           *httptest.Server
	mu            sync.Mutex
	models        []string
	kkBroken      bool
	defaultBroken bool
}

func newFallbackSTTServer(t *testing.T) *fallbackSTTServer {
	t.Helper()
	s := &fallbackSTTServer{kkBroken: true}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := formModel(r)
		s.mu.Lock()
		s.models = append(s.models, model)
		kkBroken, defBroken := s.kkBroken, s.defaultBroken
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case model == fbModelLID:
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "probe", "language": "kk", "language_probability": 0.9})
		case model == fbModelKK && kkBroken, model == fbModelDefault && defBroken:
			http.Error(w, "checkpoint not loaded", http.StatusBadRequest)
		default:
			_, _ = fmt.Fprintf(w, `{"text":"spoken by %s","language":"kk","segments":[{"start":0.0,"end":5.0,"text":"spoken by %s at some length"}]}`, model, model)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fallbackSTTServer) set(kkBroken, defaultBroken bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kkBroken, s.defaultBroken = kkBroken, defaultBroken
}

// since returns the models requested after the first n requests.
func (s *fallbackSTTServer) since(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models[n:]...)
}

func (s *fallbackSTTServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.models)
}

// retiringStore is the fake ingest store plus the two capabilities the
// §8.6.13 retirement uses, so a test can see which representations a run
// retired.
type retiringStore struct {
	fakeIngestStore
	retired []int64
}

func (s *retiringStore) ActiveRepresentations(context.Context, string) ([]store.RepresentationRow, error) {
	var out []store.RepresentationRow
	for i, rep := range s.reps {
		out = append(out, store.RepresentationRow{RepID: int64(i + 1), RepType: rep.RepType, MetaJSON: rep.MetaJSON})
	}
	return out, nil
}

func (s *retiringStore) SoftDeleteRepresentations(_ context.Context, _ string, ids []int64) (int, error) {
	s.retired = append(s.retired, ids...)
	return len(ids), nil
}

// fallbackItemHarness is an item-scope Service whose clock the test moves.
type fallbackItemHarness struct {
	svc   *ingest.Service
	store *retiringStore
	logs  *syncBuffer
	stt   *fallbackSTTServer
	state string
	now   time.Time
}

func newFallbackItemHarness(t *testing.T, onRouteError string) *fallbackItemHarness {
	t.Helper()
	stt := newFallbackSTTServer(t)
	yaml := "" +
		"stt_provider: whisper\n" +
		"providers:\n" +
		"  whisper:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + fbModelDefault + "\n" +
		"  ckpt-kk:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + fbModelKK + "\n" +
		"  lid:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + fbModelLID + "\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_identifier: lid\n" +
		"    on_route_error: " + onRouteError + "\n" +
		"    language_providers:\n" +
		"      kk: ckpt-kk\n"
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.StateDir = t.TempDir()
	cfg.QualityGatesEnabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	if cfg.MediaSTTRouteRetryAfter != 24*time.Hour {
		t.Fatalf("route_retry_after default = %v, want 24h", cfg.MediaSTTRouteRetryAfter)
	}
	h := &fallbackItemHarness{store: &retiringStore{}, logs: &syncBuffer{}, stt: stt, state: cfg.StateDir,
		now: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	h.svc = mustNewIngestService(t, cfg, h.store)
	h.svc.SetLogger(log.New(h.logs, "", 0))
	h.svc.SetClock(func() time.Time { return h.now })
	// One short recording: one STT request, and no probe cut is needed.
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 10 * time.Second, nil }
	h.svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) { return []byte("slice"), nil }
	return h
}

// fallbackMeta is the wire shape of the §8.2.4 fields on a transcript meta_json.
type fallbackMeta struct {
	Language          string `json:"language"`
	Route             string `json:"route"`
	RouteFallbackFrom string `json:"route_fallback_from"`
	RouteFallbackAt   string `json:"route_fallback_at"`
	RouteFallbackBy   string `json:"route_fallback_by"`
}

// run transcribes the item once into an empty store and returns the meta and
// the indexed text.
func (h *fallbackItemHarness) run(t *testing.T, content []byte) (fallbackMeta, string, error) {
	t.Helper()
	h.store.reps, h.store.chunks = nil, nil
	err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/kk.m4a"), content)
	if err != nil || len(h.store.reps) == 0 {
		return fallbackMeta{}, "", err
	}
	var m fallbackMeta
	if uerr := json.Unmarshal([]byte(h.store.reps[0].MetaJSON), &m); uerr != nil {
		t.Fatalf("decode meta_json: %v", uerr)
	}
	var b strings.Builder
	for _, c := range h.store.chunks {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return m, b.String(), nil
}

func (h *fallbackItemHarness) mustRun(t *testing.T, content []byte) (fallbackMeta, string) {
	t.Helper()
	m, text, err := h.run(t, content)
	if err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, h.logs.String())
	}
	return m, text
}

// seedFallback makes the first decode: the Kazakh checkpoint fails and the
// default profile decodes the item under on_route_error: default.
func (h *fallbackItemHarness) seedFallback(t *testing.T, content []byte) fallbackMeta {
	t.Helper()
	m, text := h.mustRun(t, content)
	if m.RouteFallbackFrom != "ckpt-kk" || m.Route != "whisper" || !strings.Contains(text, "spoken by "+fbModelDefault) {
		t.Fatalf("seed meta = %+v text %q: want a fallback from ckpt-kk decoded by whisper", m, text)
	}
	return m
}

// editRouteCache rewrites the cached .route.json of the only cache entry.
func (h *fallbackItemHarness) editRouteCache(t *testing.T, edit func(map[string]any)) {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(h.state, "cache", "transcribe", "*.route.json"))
	if len(paths) != 1 {
		t.Fatalf("route cache files = %v, want one", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	edit(rec)
	out, _ := json.Marshal(rec)
	if err := os.WriteFile(paths[0], out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRouteFallbackItem_StampIsRecordedPersistedAndRestored: the fallback
// decode records route_fallback_at (the clock, RFC 3339 UTC) and
// route_fallback_by (the default profile), the cache keeps both, and a cache
// hit that is not due restores them with no STT request.
func TestRouteFallbackItem_StampIsRecordedPersistedAndRestored(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("a", 4000))
	first := h.seedFallback(t, content)
	if first.RouteFallbackAt != "2026-10-18T09:00:00Z" || first.RouteFallbackBy != fbDefaultID {
		t.Fatalf("stamp = %q / %q, want 2026-10-18T09:00:00Z / %s", first.RouteFallbackAt, first.RouteFallbackBy, fbDefaultID)
	}
	paths, _ := filepath.Glob(filepath.Join(h.state, "cache", "transcribe", "*.route.json"))
	raw, _ := os.ReadFile(paths[0])
	if !strings.Contains(string(raw), `"route_fallback_at":"2026-10-18T09:00:00Z"`) || !strings.Contains(string(raw), `"route_fallback_by":"`+fbDefaultID+`"`) {
		t.Errorf("route cache %s lacks the stamp", raw)
	}

	h.now = h.now.Add(23 * time.Hour)
	before := h.stt.count()
	second, text := h.mustRun(t, content)
	if got := h.stt.since(before); len(got) != 0 {
		t.Fatalf("a fallback result that is not due made STT requests %v", got)
	}
	if second != first || !strings.Contains(text, "spoken by "+fbModelDefault) {
		t.Errorf("cache hit meta %+v differs from the decode's %+v", second, first)
	}
}

// TestRouteFallbackItem_DueRetryRecoversOnTheRoute: once route_retry_after
// has passed, the route candidate decodes the item once (no identifier, no
// default), its text replaces the fallback text, and the record and stamp go.
func TestRouteFallbackItem_DueRetryRecoversOnTheRoute(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("b", 4000))
	h.seedFallback(t, content)

	h.stt.set(false, false)
	h.now = h.now.Add(24 * time.Hour)
	before := h.stt.count()
	meta, text := h.mustRun(t, content)
	if got := h.stt.since(before); strings.Join(got, ",") != fbModelKK {
		t.Fatalf("retry requested %v, want only [%s]: the identifier is not asked again", got, fbModelKK)
	}
	if meta.Route != "ckpt-kk" || meta.RouteFallbackFrom != "" || meta.RouteFallbackAt != "" || meta.RouteFallbackBy != "" || meta.Language != "kk" {
		t.Errorf("meta = %+v, want route ckpt-kk, language kk, and no fallback record or stamp", meta)
	}
	if !strings.Contains(text, "spoken by "+fbModelKK) || strings.Contains(text, "spoken by "+fbModelDefault) {
		t.Errorf("indexed text %q: want the checkpoint's text only", text)
	}
	if !strings.Contains(h.logs.String(), `route fallback retry talks/kk.m4a: candidate "ckpt-kk" recovered`) {
		t.Errorf("no retry log line:\n%s", h.logs.String())
	}
	// The recovered text is cached: the next run decodes nothing.
	before = h.stt.count()
	h.mustRun(t, content)
	if got := h.stt.since(before); len(got) != 0 {
		t.Errorf("the run after a recovery made STT requests %v", got)
	}
}

// TestRouteFallbackItem_FailedRetryKeepsTextAndMovesTheTime: a due retry that
// fails again keeps the cached fallback text, never decodes on the default
// profile again, and sets route_fallback_at to the retry time, so the next
// retry waits a full interval.
func TestRouteFallbackItem_FailedRetryKeepsTextAndMovesTheTime(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("c", 4000))
	h.seedFallback(t, content)

	h.now = h.now.Add(25 * time.Hour)
	before := h.stt.count()
	meta, text := h.mustRun(t, content)
	if got := h.stt.since(before); strings.Join(got, ",") != fbModelKK {
		t.Fatalf("retry requested %v, want only [%s]: no second default decode", got, fbModelKK)
	}
	if meta.RouteFallbackFrom != "ckpt-kk" || meta.RouteFallbackAt != "2026-10-19T10:00:00Z" || meta.RouteFallbackBy != fbDefaultID {
		t.Errorf("meta = %+v, want the fallback record kept and route_fallback_at 2026-10-19T10:00:00Z", meta)
	}
	if !strings.Contains(text, "spoken by "+fbModelDefault) {
		t.Errorf("the cached fallback text is gone: %q", text)
	}
	if !strings.Contains(h.logs.String(), "next due 2026-10-20T10:00:00Z") {
		t.Errorf("the retry log line does not name the next due time:\n%s", h.logs.String())
	}

	h.now = h.now.Add(time.Hour)
	before = h.stt.count()
	h.mustRun(t, content)
	if got := h.stt.since(before); len(got) != 0 {
		t.Errorf("one hour after a failed retry the item was decoded again: %v", got)
	}
}

// TestRouteFallbackItem_IntervalZeroRetriesEveryRun: route_retry_after 0
// makes a fallback result due on every run.
func TestRouteFallbackItem_IntervalZeroRetriesEveryRun(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	h.svc.SetRouteRetryAfter(0)
	content := []byte(strings.Repeat("d", 4000))
	h.seedFallback(t, content)
	before := h.stt.count()
	h.mustRun(t, content)
	if got := h.stt.since(before); strings.Join(got, ",") != fbModelKK {
		t.Errorf("route_retry_after=0 requested %v, want [%s]", got, fbModelKK)
	}
}

// TestRouteFallbackItem_PolicyFlipToFailRetiresTheFallback: after the
// operator switches to on_route_error: fail, the cached fallback result is
// stale at once. Its route fails again, so the item fails with
// TRANSCRIBE_FAILED and its earlier transcript is retired; the default
// profile is not asked.
func TestRouteFallbackItem_PolicyFlipToFailRetiresTheFallback(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("e", 4000))
	h.seedFallback(t, content)

	h.svc.SetRouteErrorFallback(false)
	h.now = h.now.Add(time.Minute)
	before := h.stt.count()
	// The store still holds the earlier run's transcript, as a real store does.
	h.store.chunks = nil
	err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/kk.m4a"), content)
	if err == nil || !errors.Is(err, ingest.ErrTranscriptProviderFailure) {
		t.Fatalf("err = %v, want a transcript provider failure (TRANSCRIBE_FAILED)", err)
	}
	if got := h.stt.since(before); strings.Join(got, ",") != fbModelKK {
		t.Errorf("requested %v, want only [%s] under fail", got, fbModelKK)
	}
	if len(h.store.retired) != 1 {
		t.Errorf("retired %v, want the earlier transcript retired", h.store.retired)
	}
}

// TestRouteFallbackItem_PolicyFlipToFailRecovers: under fail a stale fallback
// result whose route works now is replaced by the route's text.
func TestRouteFallbackItem_PolicyFlipToFailRecovers(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("f", 4000))
	h.seedFallback(t, content)
	h.svc.SetRouteErrorFallback(false)
	h.stt.set(false, false)
	meta, text := h.mustRun(t, content)
	if meta.Route != "ckpt-kk" || meta.RouteFallbackFrom != "" || !strings.Contains(text, "spoken by "+fbModelKK) {
		t.Errorf("meta = %+v text %q, want the route's decode", meta, text)
	}
}

// TestRouteFallbackItem_PolicyFlipLeavesRouteDecodesAlone: a cached route
// decode holds no fallback record, so a change of policy does not re-derive it.
func TestRouteFallbackItem_PolicyFlipLeavesRouteDecodesAlone(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	h.stt.set(false, false)
	content := []byte(strings.Repeat("g", 4000))
	first, _ := h.mustRun(t, content)
	if first.Route != "ckpt-kk" || first.RouteFallbackFrom != "" {
		t.Fatalf("seed meta = %+v, want a route decode", first)
	}
	h.svc.SetRouteErrorFallback(false)
	h.stt.set(true, false)
	before := h.stt.count()
	h.mustRun(t, content)
	if got := h.stt.since(before); len(got) != 0 {
		t.Errorf("a route decode was re-derived after the policy flip: %v", got)
	}
}

// TestRouteFallbackItem_ProducerChangeIsStaleAtOnce: a fallback result whose
// route_fallback_by is absent (a cache entry from before spec 0.82.0) or names
// another profile is stale on the next run, not after route_retry_after. The
// route decodes first; when it fails, the current default decodes once and the
// record names the current producer.
func TestRouteFallbackItem_ProducerChangeIsStaleAtOnce(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"absent":  func(rec map[string]any) { delete(rec, "route_fallback_by") },
		"changed": func(rec map[string]any) { rec["route_fallback_by"] = "whisper|old-model" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newFallbackItemHarness(t, "default")
			content := []byte(strings.Repeat("h", 4000))
			h.seedFallback(t, content)
			h.editRouteCache(t, edit)

			h.now = h.now.Add(time.Hour)
			before := h.stt.count()
			meta, _ := h.mustRun(t, content)
			if got := h.stt.since(before); strings.Join(got, ",") != fbModelKK+","+fbModelDefault {
				t.Fatalf("requested %v, want [%s %s]: the route first, then the default once", got, fbModelKK, fbModelDefault)
			}
			if meta.RouteFallbackFrom != "ckpt-kk" || meta.RouteFallbackBy != fbDefaultID || meta.RouteFallbackAt != "2026-10-18T10:00:00Z" {
				t.Errorf("meta = %+v, want the record naming the current producer at the retry time", meta)
			}
		})
	}
}

// TestRouteFallbackItem_DueGate: the §8.6.7 gate reads the stored meta_json
// and processes an unchanged document again only when its fallback result is
// stale or due.
func TestRouteFallbackItem_DueGate(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	meta := func(from, at, by string) string {
		raw, _ := json.Marshal(map[string]string{"route_fallback_from": from, "route_fallback_at": at, "route_fallback_by": by})
		return string(raw)
	}
	cases := []struct {
		name string
		meta string
		fail bool
		want bool
	}{
		{"no fallback record", meta("", "2026-10-01T00:00:00Z", fbDefaultID), false, false},
		{"not due", meta("ckpt-kk", "2026-10-18T00:00:00Z", fbDefaultID), false, false},
		{"due", meta("ckpt-kk", "2026-10-17T09:00:00Z", fbDefaultID), false, true},
		{"no time", meta("ckpt-kk", "", fbDefaultID), false, true},
		{"bad time", meta("ckpt-kk", "yesterday", fbDefaultID), false, true},
		{"no producer", meta("ckpt-kk", "2026-10-18T00:00:00Z", ""), false, true},
		{"other producer", meta("ckpt-kk", "2026-10-18T00:00:00Z", "whisper|old"), false, true},
		{"policy fail", meta("ckpt-kk", "2026-10-18T00:00:00Z", fbDefaultID), true, true},
		{"window fallback", `{"coverage":{"windows_attempted":2,"languages":[{"start_ms":0,"end_ms":5,"route":"whisper","covered":true,"fallback_from":"ckpt-kk"}]},"route_fallback_at":"2026-10-17T09:00:00Z","route_fallback_by":"` + fbDefaultID + `"}`, false, true},
	}
	for _, c := range cases {
		h.svc.SetRouteErrorFallback(!c.fail)
		if got, why := h.svc.RouteFallbackDue(c.meta); got != c.want {
			t.Errorf("%s: due = %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}
