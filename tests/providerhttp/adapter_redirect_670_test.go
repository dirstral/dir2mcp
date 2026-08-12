package providerhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/anthropic"
	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/colbertrerank"
	"github.com/dirstral/dir2mcp/internal/elevenlabs"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/omniembed"
	"github.com/dirstral/dir2mcp/internal/openai"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
)

// testSecret is the fake API key that every adapter under test sends. The
// redirect target watches for it. A real key never appears in a test.
const testSecret = "secret-670-must-not-leak"

// adapterCall drives one provider adapter against a base URL.
type adapterCall struct {
	name string
	call func(ctx context.Context, baseURL string) error
}

// adapterCalls returns one entry per provider adapter. Retries are turned off,
// so a failing call returns at once.
func adapterCalls() []adapterCall {
	return []adapterCall{
		{"anthropic.Generate", func(ctx context.Context, base string) error {
			c := anthropic.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Generate(ctx, "hello")
			return err
		}},
		{"cohere.Rerank", func(ctx context.Context, base string) error {
			c := cohere.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Rerank(ctx, "", "q", []string{"doc"}, 1)
			return err
		}},
		{"cohere.Embed", func(ctx context.Context, base string) error {
			c := cohere.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Embed(ctx, "", model.EmbedDocument, []string{"doc"})
			return err
		}},
		{"cohere.Generate", func(ctx context.Context, base string) error {
			c := cohere.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Generate(ctx, "hello")
			return err
		}},
		{"colbertrerank.Rerank", func(ctx context.Context, base string) error {
			c := colbertrerank.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Rerank(ctx, "", "q", []string{"doc"}, 1)
			return err
		}},
		{"whisperapi.TranscribeStructured", func(ctx context.Context, base string) error {
			c := whisperapi.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.TranscribeStructured(ctx, "clip.mp3", []byte("audio"))
			return err
		}},
		{"elevenlabs.Transcribe", func(ctx context.Context, base string) error {
			c := elevenlabs.NewClient(testSecret, "voice-670")
			c.BaseURL = base
			_, err := c.Transcribe(ctx, "clip.mp3", []byte("audio"))
			return err
		}},
		{"elevenlabs.Synthesize", func(ctx context.Context, base string) error {
			c := elevenlabs.NewClient(testSecret, "voice-670")
			c.BaseURL = base
			_, err := c.Synthesize(ctx, "hello")
			return err
		}},
		{"gemini.Generate", func(ctx context.Context, base string) error {
			c := gemini.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Generate(ctx, "hello")
			return err
		}},
		{"mistral.Embed", func(ctx context.Context, base string) error {
			c := mistral.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Embed(ctx, "", model.EmbedDocument, []string{"doc"})
			return err
		}},
		{"omniembed.Embed", func(ctx context.Context, base string) error {
			c := omniembed.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Embed(ctx, "", model.EmbedDocument, []string{"doc"})
			return err
		}},
		{"openai.Embed", func(ctx context.Context, base string) error {
			c := openai.NewClient(base, testSecret)
			c.MaxRetries = 0
			_, err := c.Embed(ctx, "", model.EmbedDocument, []string{"doc"})
			return err
		}},
	}
}

// redirectWatcher records what a redirect target receives.
type redirectWatcher struct {
	mu      sync.Mutex
	hits    int
	headers []string
}

func (w *redirectWatcher) record(r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hits++
	for name, values := range r.Header {
		for _, v := range values {
			if v == testSecret || v == "Bearer "+testSecret {
				w.headers = append(w.headers, name)
			}
		}
	}
}

func (w *redirectWatcher) result() (int, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hits, append([]string(nil), w.headers...)
}

// TestAdaptersRefuseARedirect pins the credential half of issue #670. Go strips
// Authorization only when a redirect crosses to another host, and it never
// strips a custom key header such as x-api-key, xi-api-key or x-goog-api-key.
// A redirect from a compromised or misconfigured endpoint would therefore hand
// the API key to the redirect target. Every adapter must stop at the 3xx.
func TestAdaptersRefuseARedirect(t *testing.T) {
	for _, tc := range adapterCalls() {
		t.Run(tc.name, func(t *testing.T) {
			watcher := &redirectWatcher{}
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				watcher.record(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer target.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer origin.Close()

			err := tc.call(context.Background(), origin.URL)
			if err == nil {
				t.Fatal("the call must fail on a redirect")
			}
			hits, leaked := watcher.result()
			if hits != 0 {
				t.Fatalf("the redirect target got %d requests, want 0", hits)
			}
			if len(leaked) != 0 {
				t.Fatalf("the credential reached the redirect target in header(s) %v", leaked)
			}
		})
	}
}
