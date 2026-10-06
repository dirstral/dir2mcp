package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/quality"
)

// SPEC §8.2.4 (dir2mcp #1059, #1060). Under the default item scope a bound
// language identifier resolves the recording's language BEFORE transcription
// and the first eligible language_providers candidate decodes it; the default
// profile decodes an unmatched or unknown language; under
// media.stt.on_route_error: default a route that fails is replaced once by the
// default profile. The fake STT server below is the OpenAI-compatible
// /v1/audio/transcriptions shape a local whisper server exposes: it selects the
// checkpoint by the `model` form field, which is what these tests observe.

const (
	modelDefault = "base-model"
	modelFA      = "ckpt-fa"
	modelBroken  = "ckpt-broken"
	modelLID     = "lid-model"
)

// routeSTTServer is a fake whisper server that records the `model` of every
// request in order and answers per model: the identifier model reports the
// scripted language, the broken model fails, the others return a transcript
// that names the model that produced it.
type routeSTTServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	// models is every `model` field received, in request order.
	models []string
	// lidLang / lidConf is what the identifier model reports; lidFail makes
	// the identifier request fail.
	lidLang string
	lidConf float64
	lidFail bool
}

func newRouteSTTServer(t *testing.T) *routeSTTServer {
	t.Helper()
	s := &routeSTTServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		model := formModel(r)
		s.mu.Lock()
		s.models = append(s.models, model)
		lang, conf, fail := s.lidLang, s.lidConf, s.lidFail
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch model {
		case modelLID:
			if fail {
				http.Error(w, "identifier down", http.StatusBadRequest)
				return
			}
			body := map[string]any{"text": "probe text", "language": lang}
			if conf > 0 {
				body["language_probability"] = conf
			}
			_ = json.NewEncoder(w).Encode(body)
		case modelBroken:
			// 400 is not retried by the client, so the fallback is observed
			// without a backoff wait.
			http.Error(w, "checkpoint not loaded", http.StatusBadRequest)
		default:
			_, _ = fmt.Fprintf(w, `{"text":"spoken by %s","language":"xx","segments":[{"start":0.0,"end":5.0,"text":"spoken by %s at some length"}]}`, model, model)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func formModel(r *http.Request) string {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" {
			data, _ := io.ReadAll(part)
			return string(data)
		}
	}
}

func (s *routeSTTServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

func (s *routeSTTServer) setLID(lang string, conf float64, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lidLang, s.lidConf, s.lidFail = lang, conf, fail
}

// itemRouteHarness builds a Service from a real config file: the built-in
// whisper profile is the default (model base-model), two user profiles are
// per-language checkpoints, and `lid` is the identifier. Every profile points
// at the fake server.
type itemRouteHarness struct {
	svc   *ingest.Service
	store *fakeIngestStore
	logs  *syncBuffer
	stt   *routeSTTServer
	cuts  []sttWindowCut
	mu    sync.Mutex
}

// defaultProfileExtra is appended to the default profile's block, so a test
// can pin its language (stt_language) the way an operator does.
func newItemRouteHarness(t *testing.T, onRouteError string, defaultProfileExtra string) *itemRouteHarness {
	t.Helper()
	stt := newRouteSTTServer(t)
	stateDir := t.TempDir()
	yaml := "" +
		"stt_provider: whisper\n" +
		"providers:\n" +
		"  whisper:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + modelDefault + "\n" +
		defaultProfileExtra +
		"  ckpt-fa:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + modelFA + "\n" +
		"    stt_languages: [fa]\n" +
		"    stt_validation:\n" +
		"      - {language: fa, method: wer, sample: 100 read sentences, score: \"13.3\", date: 2026-10-06}\n" +
		"  ckpt-broken:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + modelBroken + "\n" +
		"  lid:\n" +
		"    kind: whisper\n" +
		"    base_url: " + stt.srv.URL + "\n" +
		"    stt_model: " + modelLID + "\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_identifier: lid\n" +
		"    language_probe_sec: 20\n" +
		"    on_route_error: " + onRouteError + "\n" +
		"    language_providers:\n" +
		"      fa: ckpt-fa\n" +
		"      kk: ckpt-broken\n"
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.StateDir = stateDir
	cfg.QualityGatesEnabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	h := &itemRouteHarness{store: &fakeIngestStore{}, logs: &syncBuffer{}, stt: stt}
	h.svc = mustNewIngestService(t, cfg, h.store)
	h.svc.SetLogger(log.New(h.logs, "", 0))
	// A one-minute recording: one STT request, longer than the 20 s probe, so
	// the identifier gets a centred slice cut through ExtractSegmentFunc.
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return time.Minute, nil }
	h.svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		h.mu.Lock()
		h.cuts = append(h.cuts, sttWindowCut{startMS: startMS, endMS: endMS})
		h.mu.Unlock()
		return []byte("probe-slice"), nil
	}
	return h
}

// itemRouteMeta is the wire shape of the §8.2.4 fields on a transcript meta_json.
type itemRouteMeta struct {
	Language           string   `json:"language"`
	LanguageSource     string   `json:"language_source"`
	LanguageConfidence *float64 `json:"language_confidence"`
	LanguageCovered    *bool    `json:"language_covered"`
	LanguageScope      string   `json:"language_scope"`
	LanguageRoutes     string   `json:"language_routes"`
	LanguageIdentifier string   `json:"language_identifier"`
	Route              string   `json:"route"`
	RouteFallbackFrom  string   `json:"route_fallback_from"`
	Provider           string   `json:"provider"`
	Model              string   `json:"model"`
}

func (h *itemRouteHarness) run(t *testing.T, relPath string, content []byte) itemRouteMeta {
	t.Helper()
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc(relPath), content); err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, h.logs.String())
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want 1\nlogs:\n%s", len(h.store.reps), h.logs.String())
	}
	var m itemRouteMeta
	if err := json.Unmarshal([]byte(h.store.reps[0].MetaJSON), &m); err != nil {
		t.Fatalf("decode meta_json %q: %v", h.store.reps[0].MetaJSON, err)
	}
	return m
}

func (h *itemRouteHarness) text() string {
	var b strings.Builder
	for _, c := range h.store.chunks {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestItemRoute_ConfiguredLanguagePicksTheCheckpoint: the identifier reports
// Persian, so the Persian checkpoint decodes the whole item and the record
// says so.
//
// Mutant killed: routing on the pin only (the default model decodes).
func TestItemRoute_ConfiguredLanguagePicksTheCheckpoint(t *testing.T) {
	h := newItemRouteHarness(t, "fail", "")
	h.stt.setLID("fa", 0.93, false)
	content := []byte(strings.Repeat("a", 4000))

	meta := h.run(t, "talks/one.m4a", content)
	if got := h.stt.seen(); strings.Join(got, ",") != modelLID+","+modelFA {
		t.Fatalf("models requested = %v, want [%s %s]: the identifier, then the Persian checkpoint only", got, modelLID, modelFA)
	}
	if meta.Route != "ckpt-fa" || meta.Language != "fa" || meta.LanguageSource != "detected" || meta.LanguageConfidence == nil || *meta.LanguageConfidence != 0.93 {
		t.Errorf("meta = %+v, want route ckpt-fa, language fa detected at 0.93", meta)
	}
	assertItemIdentityFields(t, meta)
	if meta.LanguageCovered != nil {
		t.Errorf("language_covered = %v, want absent: ckpt-fa declares fa", *meta.LanguageCovered)
	}
	if !strings.Contains(h.text(), "spoken by "+modelFA) {
		t.Errorf("the checkpoint's text is not in the transcript:\n%s", h.text())
	}
	h.assertProbeCut(t)
}

// assertItemIdentityFields checks the §8.2.4 fields that join the derivation
// identity, and that provider/model keep naming the default profile.
func assertItemIdentityFields(t *testing.T, meta itemRouteMeta) {
	t.Helper()
	if meta.LanguageIdentifier != "lid" || meta.LanguageScope != "item" || !strings.Contains(meta.LanguageRoutes, "fa=ckpt-fa|"+modelFA) {
		t.Errorf("identity fields = identifier %q scope %q routes %q", meta.LanguageIdentifier, meta.LanguageScope, meta.LanguageRoutes)
	}
	if meta.Provider != "whisper" || meta.Model != modelDefault {
		t.Errorf("provider/model = %q/%q: the §8.6.7 identity stays the default profile; the route is recorded separately", meta.Provider, meta.Model)
	}
}

// assertProbeCut checks that the identifier heard one centred 20 s slice of
// the one-minute recording.
func (h *itemRouteHarness) assertProbeCut(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	cuts := append([]sttWindowCut(nil), h.cuts...)
	h.mu.Unlock()
	if len(cuts) != 1 || cuts[0].startMS != 20_000 || cuts[0].endMS != 40_000 {
		t.Errorf("identifier probe cuts = %+v, want one centred 20 s slice [20000,40000]", cuts)
	}
}

// TestItemRoute_UnconfiguredLanguagePicksTheDefault: Russian has no route, so
// the default profile decodes; the identifier's language is still recorded.
func TestItemRoute_UnconfiguredLanguagePicksTheDefault(t *testing.T) {
	h := newItemRouteHarness(t, "fail", "")
	h.stt.setLID("ru", 0.8, false)

	meta := h.run(t, "talks/two.m4a", []byte(strings.Repeat("b", 4000)))
	if got := h.stt.seen(); strings.Join(got, ",") != modelLID+","+modelDefault {
		t.Fatalf("models requested = %v, want [%s %s]", got, modelLID, modelDefault)
	}
	if meta.Route != "whisper" || meta.Language != "ru" || meta.RouteFallbackFrom != "" {
		t.Errorf("meta = %+v, want route whisper (the default), language ru, no fallback", meta)
	}
}

// TestItemRoute_UnknownLanguagePicksTheDefault: an identifier that fails, or
// reports nothing, or reports below the floor, is no signal: the default
// profile decodes, no route is recorded, and the item never fails.
func TestItemRoute_UnknownLanguagePicksTheDefault(t *testing.T) {
	for name, lid := range map[string]struct {
		lang string
		conf float64
		fail bool
	}{
		"error":          {fail: true},
		"empty":          {},
		"low confidence": {lang: "fa", conf: 0.2},
	} {
		t.Run(name, func(t *testing.T) {
			h := newItemRouteHarness(t, "fail", "")
			h.stt.setLID(lid.lang, lid.conf, lid.fail)
			meta := h.run(t, "talks/three.m4a", []byte(strings.Repeat("c", 4000)))
			if got := h.stt.seen(); strings.Join(got, ",") != modelLID+","+modelDefault {
				t.Fatalf("models requested = %v, want [%s %s]", got, modelLID, modelDefault)
			}
			if meta.Route != "" || meta.RouteFallbackFrom != "" || meta.Language == "fa" {
				t.Errorf("meta = %+v, want no route and not the identifier's language", meta)
			}
			if meta.LanguageIdentifier != "lid" || meta.LanguageScope != "item" {
				t.Errorf("the identity fields must be recorded even with no signal: %+v", meta)
			}
		})
	}
}

// TestItemRoute_CheckpointErrorFallsBackOnce: the Kazakh route fails; under
// on_route_error=default the default profile decodes the item once and the
// record names the failed candidate.
//
// Mutant killed: dropping the fallback (the item fails), or recording the
// default without route_fallback_from.
func TestItemRoute_CheckpointErrorFallsBackOnce(t *testing.T) {
	h := newItemRouteHarness(t, "default", "")
	h.stt.setLID("kk", 0.9, false)

	meta := h.run(t, "talks/four.m4a", []byte(strings.Repeat("d", 4000)))
	if got := h.stt.seen(); strings.Join(got, ",") != modelLID+","+modelBroken+","+modelDefault {
		t.Fatalf("models requested = %v, want [%s %s %s]: the broken checkpoint once, then the default once", got, modelLID, modelBroken, modelDefault)
	}
	if meta.Route != "whisper" || meta.RouteFallbackFrom != "ckpt-broken" || meta.Language != "kk" {
		t.Errorf("meta = %+v, want route whisper with route_fallback_from ckpt-broken, language kk", meta)
	}
	if !strings.Contains(h.logs.String(), "media.stt.on_route_error=default") {
		t.Errorf("no log line named the fallback:\n%s", h.logs.String())
	}
}

// TestItemRoute_CheckpointErrorFailsUnderFail: the default policy keeps the
// failed item, exactly as before §8.2.4.
func TestItemRoute_CheckpointErrorFailsUnderFail(t *testing.T) {
	h := newItemRouteHarness(t, "fail", "")
	h.stt.setLID("kk", 0.9, false)

	err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/five.m4a"), []byte(strings.Repeat("e", 4000)))
	if err == nil || !strings.Contains(err.Error(), "checkpoint not loaded") {
		t.Fatalf("err = %v, want the checkpoint's failure", err)
	}
	if got := h.stt.seen(); strings.Join(got, ",") != modelLID+","+modelBroken {
		t.Errorf("models requested = %v, want [%s %s]: no decode on the default", got, modelLID, modelBroken)
	}
	if len(h.store.reps) != 0 {
		t.Errorf("persisted %d representations, want none", len(h.store.reps))
	}
}

// TestItemRoute_CacheHitRestoresTheRoute: a second run over the same bytes
// makes no STT request and records the same route, so the transcript's
// language and route do not change between runs.
func TestItemRoute_CacheHitRestoresTheRoute(t *testing.T) {
	h := newItemRouteHarness(t, "fail", "")
	h.stt.setLID("fa", 0.93, false)
	content := []byte(strings.Repeat("f", 4000))
	first := h.run(t, "talks/six.m4a", content)
	before := len(h.stt.seen())

	h.store.reps, h.store.chunks = nil, nil
	second := h.run(t, "talks/six.m4a", content)
	if len(h.stt.seen()) != before {
		t.Fatalf("the second run made %d STT requests, want none (cache hit)", len(h.stt.seen())-before)
	}
	if second.Route != first.Route || second.Language != first.Language || second.LanguageSource != first.LanguageSource {
		t.Errorf("cache hit meta %+v differs from the decode's %+v", second, first)
	}
}

// TestItemRoute_PinDisablesTheIdentifier: an operator pin applies to every
// item, so the identifier is never asked and the pin routes the corpus as
// §8.2.1 did.
func TestItemRoute_PinDisablesTheIdentifier(t *testing.T) {
	h := newItemRouteHarness(t, "fail", "    stt_language: fa\n")
	h.stt.setLID("ru", 0.9, false)
	meta := h.run(t, "talks/seven.m4a", []byte(strings.Repeat("g", 4000)))
	for _, m := range h.stt.seen() {
		if m == modelLID {
			t.Fatalf("the identifier was asked under a pin: %v", h.stt.seen())
		}
	}
	if meta.Language != "fa" || meta.LanguageSource != "configured" || meta.Route != "" {
		t.Errorf("meta = %+v, want the pinned fa, configured, and no §8.2.4 route record", meta)
	}
}

// TestWindowRoute_AFailingCandidateFallsBackToDefault is the window-scope
// half of §8.2.4: a candidate that errors after a refusal is replaced once by
// the default profile, and the entry names it in fallback_from.
func TestWindowRoute_AFailingCandidateFallsBackToDefault(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	h.svc.SetRouteErrorFallback(true)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}}, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{err: errors.New("503")}}, "ru-second", nil)

	meta := runScoped(t, h, "talks/fallback.m4a", content)
	if len(meta.Coverage.Refused) != 0 {
		t.Errorf("refused = %+v, want none: the default decoded every window after the candidate failed", meta.Coverage.Refused)
	}
	if len(meta.Coverage.Languages) == 0 {
		t.Fatal("no coverage.languages recorded")
	}
	for _, e := range meta.Coverage.Languages {
		if e.Route != "whisper" || e.FallbackFrom != "ru-second" {
			t.Errorf("entry %+v: want route whisper with fallback_from ru-second", e)
		}
	}
	if !strings.Contains(h.logs.String(), "media.stt.on_route_error=default") {
		t.Errorf("no log line named the fallback:\n%s", h.logs.String())
	}
}

// TestWindowRoute_FallbackFromJoinsTheCoalescingKey pins the §8.2.2 record
// rule for the new field: a fallen-back stretch is its own entry.
func TestWindowRoute_FallbackFromJoinsTheCoalescingKey(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	h.svc.SetRouteErrorFallback(true)
	// The first candidate loops only on the second window; the second candidate
	// always errors. Windows 1, 3, 4 decode on ru-first; window 2 falls back.
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{2: {lang: "ru", conf: 0.9, text: loopText}}}, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{err: errors.New("503")}}, "ru-second", nil)

	meta := runScoped(t, h, "talks/coalesce.m4a", content)
	var fallen int
	for _, e := range meta.Coverage.Languages {
		if e.FallbackFrom != "" {
			fallen++
			if e.Route != "whisper" || e.FallbackFrom != "ru-second" {
				t.Errorf("fallen-back entry %+v: want route whisper from ru-second", e)
			}
		}
	}
	if fallen != 1 {
		t.Errorf("coverage.languages = %+v: want exactly one fallen-back entry, got %d", meta.Coverage.Languages, fallen)
	}
}
