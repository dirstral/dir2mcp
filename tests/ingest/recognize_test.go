package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakeRecognizer is a canned model.Recognizer double (design 0004).
type fakeRecognizer struct {
	result      model.RecognizeResult
	err         error
	calls       int
	lastAbsPath string
}

func (f *fakeRecognizer) Recognize(_ context.Context, absPath string) (model.RecognizeResult, error) {
	f.calls++
	f.lastAbsPath = absPath
	if f.err != nil {
		return model.RecognizeResult{}, f.err
	}
	return f.result, nil
}

func recognizeTestResult() model.RecognizeResult {
	return model.RecognizeResult{
		Name:    "dirstral-annotator",
		Version: "0.2.0",
		Annotations: []model.RecognizedAnnotation{
			{
				StartMS: 2530000, EndMS: 2551000, Event: "pitch",
				Entities:   []string{"player:webb-logan"},
				Text:       "Pitch: Logan Webb to Freddie Freeman — fly out",
				Confidence: 0.97, Sources: []string{"scorebug", "face"},
			},
			{
				StartMS: 3785500, EndMS: 3792000, Event: "pitch",
				Entities:   []string{"player:webb-logan"},
				Text:       "Pitch: Logan Webb to Mookie Betts — swinging strike",
				Confidence: 0.98, Sources: []string{"playbyplay"},
			},
		},
	}
}

func TestRecognize_GeneratesRecognitionRepresentation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "games", "game7.mp4"), "fake-video")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	rec := &fakeRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("expected exactly one backend call, got %d", rec.calls)
	}
	if want := filepath.Join(root, "games", "game7.mp4"); rec.lastAbsPath != want {
		t.Fatalf("backend must receive the absolute media path, got %q want %q", rec.lastAbsPath, want)
	}

	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeRecognition {
		t.Fatalf("expected one recognition representation, got %+v", st.reps)
	}
	// Derivation-identity meta (design 0004 §4): source + backend name/version.
	meta := st.reps[0].MetaJSON
	for _, want := range []string{`"source":"recognize"`, `"model":"dirstral-annotator"`, `"model_version":"0.2.0"`} {
		if !strings.Contains(meta, want) {
			t.Fatalf("recognition meta must contain %s, got %s", want, meta)
		}
	}

	// One chunk per annotation, statement text preserved (this is what makes
	// "find all pitches by player X" a plain search query).
	if len(st.chunks) != 2 {
		t.Fatalf("expected one chunk per annotation, got %d", len(st.chunks))
	}
	if !strings.Contains(st.chunks[0].Text, "Logan Webb to Freddie Freeman") ||
		!strings.Contains(st.chunks[1].Text, "Logan Webb to Mookie Betts") {
		t.Fatalf("chunk texts must carry the statements, got %q / %q", st.chunks[0].Text, st.chunks[1].Text)
	}

	// Each chunk carries exactly one `time` span with the annotation window.
	if len(st.spans) != 2 {
		t.Fatalf("expected two time spans, got %+v", st.spans)
	}
	for i, want := range []struct{ start, end int }{{2530000, 2551000}, {3785500, 3792000}} {
		sp := st.spans[i]
		if sp.Kind != "time" || sp.StartMS != want.start || sp.EndMS != want.end {
			t.Fatalf("span %d: want time %d..%d, got %+v", i, want.start, want.end, sp)
		}
	}
}

func TestRecognize_NoOpWithoutRecognizerOrForNonVideo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)

	// No recognizer bound (recognize.provider=off): video is a no-op.
	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("no-recognizer no-op errored: %v", err)
	}

	// Recognizer bound, but the document is audio: v1 recognizes video only.
	rec := &fakeRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)
	audio := model.Document{DocID: 2, RelPath: "talk.mp3", DocType: "audio"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), audio); err != nil {
		t.Fatalf("non-video no-op errored: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("backend must not be called for non-video docs, got %d call(s)", rec.calls)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representations, got %+v", st.reps)
	}
}

func TestRecognize_EmptyOrBlankAnnotationsPersistNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	svc.SetRecognizer(&fakeRecognizer{result: model.RecognizeResult{
		Name: "r", Version: "1",
		Annotations: []model.RecognizedAnnotation{{StartMS: 0, EndMS: 1000, Text: "   "}},
	}})

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if len(st.reps) != 0 || len(st.chunks) != 0 {
		t.Fatalf("blank annotations must persist nothing, got reps=%d chunks=%d", len(st.reps), len(st.chunks))
	}
}

func TestRecognize_BackendErrorPropagates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	svc.SetRecognizer(&fakeRecognizer{err: errors.New("backend down")})

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Fatalf("expected backend error to propagate (STT parity), got %v", err)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representation on backend error, got %+v", st.reps)
	}
}

func TestRecognizeServeClient_WireContract(t *testing.T) {
	t.Parallel()
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"recognizer": map[string]string{"name": "dirstral-annotator", "version": "0.2.0"},
			"annotations": []map[string]any{{
				"start_s": 2530.0, "end_s": 2551.0, "event": "pitch",
				"entities":   []string{"player:webb-logan"},
				"text":       "Pitch: Logan Webb to Freddie Freeman — fly out",
				"confidence": 0.97, "sources": []string{"scorebug", "face"},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	// Trailing slash must be tolerated (docling-serve URL-hygiene parity).
	client := ingest.NewRecognizeServeClient(srv.URL + "/")
	res, err := client.Recognize(context.Background(), "/corpus/games/game7.mp4")
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if gotPath != "/recognize" {
		t.Fatalf("expected POST /recognize, got %q", gotPath)
	}
	if !strings.Contains(gotBody, `"path":"/corpus/games/game7.mp4"`) {
		t.Fatalf("request body must carry the media path, got %s", gotBody)
	}
	if res.Name != "dirstral-annotator" || res.Version != "0.2.0" {
		t.Fatalf("recognizer identity not decoded: %+v", res)
	}
	if len(res.Annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %+v", res.Annotations)
	}
	ann := res.Annotations[0]
	if ann.StartMS != 2530000 || ann.EndMS != 2551000 {
		t.Fatalf("seconds must convert to ms, got %d..%d", ann.StartMS, ann.EndMS)
	}
	if ann.Confidence != 0.97 || ann.Event != "pitch" {
		t.Fatalf("annotation fields not decoded: %+v", ann)
	}
}

func TestRecognizeServeClient_ErrorStatusHasNoBodyEcho(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stack trace with /secret/local/path", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := ingest.NewRecognizeServeClient(srv.URL).Recognize(context.Background(), "/x.mp4")
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error must not echo the response body, got %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should carry the status code, got %v", err)
	}
}

func TestRecognizeConfig_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider, url string
		wantErr       string
	}{
		{"off", "", ""},
		{"", "", ""},
		{"serve", "http://127.0.0.1:8765", ""},
		{"serve", "", "recognize.base_url"},
		{"serve", "not-a-url", "recognize.base_url"},
		{"bogus", "", "recognize.provider"},
	}
	for _, tc := range cases {
		cfg := config.Default()
		cfg.RecognizeProvider = tc.provider
		cfg.RecognizeServeURL = tc.url
		err := cfg.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("provider=%q url=%q: unexpected error %v", tc.provider, tc.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "CONFIG_INVALID") {
			t.Fatalf("provider=%q url=%q: want CONFIG_INVALID mentioning %q, got %v", tc.provider, tc.url, tc.wantErr, err)
		}
	}
}
