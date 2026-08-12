package providerhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/anthropic"
	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/colbertrerank"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
)

// jsonCapBytes mirrors providerhttp.MaxJSONResponseBytes. It is repeated here
// as a literal so this file also compiles against a tree without the shared
// package, which is how the regression was proved on main.
// TestJSONHelperUsesTheJSONCap pins the two values together.
const jsonCapBytes = 64 << 20

// oversizedCase drives one adapter call against a 200 response that is larger
// than the cap but otherwise valid.
type oversizedCase struct {
	name string
	// body is the JSON that follows the oversized padding field. It must
	// describe a successful response for the adapter under test, so that a
	// client without the cap decodes it and returns no error.
	body string
	call func(ctx context.Context, baseURL string) error
}

func oversizedCases() []oversizedCase {
	return []oversizedCase{
		{
			name: "anthropic.Generate",
			body: `"content":[{"type":"text","text":"ok"}]}`,
			call: func(ctx context.Context, base string) error {
				c := anthropic.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.Generate(ctx, "hello")
				return err
			},
		},
		{
			name: "cohere.Rerank",
			body: `"results":[{"index":0,"relevance_score":0.5}]}`,
			call: func(ctx context.Context, base string) error {
				c := cohere.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.Rerank(ctx, "", "q", []string{"doc"}, 1)
				return err
			},
		},
		{
			name: "cohere.Embed",
			body: `"embeddings":{"float":[[0.5,0.5]]}}`,
			call: func(ctx context.Context, base string) error {
				c := cohere.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.Embed(ctx, "", model.EmbedDocument, []string{"doc"})
				return err
			},
		},
		{
			name: "cohere.Generate",
			body: `"message":{"content":[{"type":"text","text":"ok"}]}}`,
			call: func(ctx context.Context, base string) error {
				c := cohere.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.Generate(ctx, "hello")
				return err
			},
		},
		{
			name: "colbertrerank.Rerank",
			body: `"results":[{"index":0,"relevance_score":0.5}]}`,
			call: func(ctx context.Context, base string) error {
				c := colbertrerank.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.Rerank(ctx, "", "q", []string{"doc"}, 1)
				return err
			},
		},
		{
			name: "whisperapi.TranscribeStructured",
			body: `"text":"ok"}`,
			call: func(ctx context.Context, base string) error {
				c := whisperapi.NewClient(base, testSecret)
				c.MaxRetries = 0
				_, err := c.TranscribeStructured(ctx, "clip.mp3", []byte("audio"))
				return err
			},
		},
	}
}

// writeOversizedJSON streams a valid JSON object whose first field holds more
// than jsonCapBytes of padding. It writes in 1 MiB chunks, so the test itself
// never holds the whole body in memory.
func writeOversizedJSON(w http.ResponseWriter, tail string) {
	const chunkSize = 1 << 20
	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = 'a'
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"pad":"`)); err != nil {
		return
	}
	for written := int64(0); written <= jsonCapBytes; written += chunkSize {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
	_, _ = w.Write([]byte(`",`))
	_, _ = w.Write([]byte(tail))
}

// TestAdaptersRejectAnOversizedSuccessBody pins the memory half of issue #670.
// A 2xx body was decoded straight from resp.Body, so a hostile or broken
// endpoint could return an endless (or gzip-bombed) success response and drive
// the daemon out of memory. Every adapter must stop at the cap instead.
//
// Each case returns a body that is valid for the adapter, so an unbounded
// client returns success. The test asserts an error, which is why it fails
// without the fix.
func TestAdaptersRejectAnOversizedSuccessBody(t *testing.T) {
	for _, tc := range oversizedCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeOversizedJSON(w, tc.body)
			}))
			defer srv.Close()

			err := tc.call(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("an oversized success body must fail")
			}
			var pErr *model.ProviderError
			if !errors.As(err, &pErr) {
				t.Fatalf("want a *model.ProviderError, got %T: %v", err, err)
			}
			if pErr.Retryable {
				t.Fatal("an oversized body must be non-retryable")
			}
			if strings.Contains(pErr.Message, "aaa") {
				t.Fatalf("the error message leaks body content: %q", pErr.Message)
			}
		})
	}
}
