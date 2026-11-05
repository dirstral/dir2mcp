package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
)

// SPEC §8.6.13 (spec 0.88.0, dir2mcp #1143): the low-confidence re-decode asks
// the same route and model for one window in the item language. ForLanguage
// returns a copy that sends the hint; the receiver keeps sending none.
func TestForLanguage_SendsTheHintOnACopyOnly(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		mu.Lock()
		seen = append(seen, r.FormValue("language")+"|"+r.FormValue("model"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok","segments":[{"start":0,"end":1,"text":"ok"}]}`))
	}))
	defer srv.Close()

	c := whisperapi.NewClient(srv.URL, "")
	c.DefaultModel = "large-v3"
	hinted, ok := model.TranscriberForLanguage(c, "uk")
	if !ok {
		t.Fatal("the whisper client does not take a language hint")
	}
	if _, err := hinted.Transcribe(context.Background(), "a.wav", []byte("audio")); err != nil {
		t.Fatalf("hinted transcribe: %v", err)
	}
	if _, err := c.Transcribe(context.Background(), "a.wav", []byte("audio")); err != nil {
		t.Fatalf("plain transcribe: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "uk|large-v3" || seen[1] != "|large-v3" {
		t.Fatalf("requests sent language|model = %v, want [uk|large-v3 |large-v3]", seen)
	}
	if _, ok := model.TranscriberForLanguage(c, ""); ok {
		t.Error("an empty language must not count as a hint")
	}
}
