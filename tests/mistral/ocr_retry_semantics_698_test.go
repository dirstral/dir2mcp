package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

// #698: OCR read MaxRetries as a TOTAL attempt count and looped below it,
// while embed, transcribe and generate each make one attempt plus MaxRetries
// retries. So MaxRetries=1 disabled OCR retry and gave every other operation
// one retry, and the default 3 gave OCR three attempts against their four.
//
// The pre-existing OCR retry test did not catch this. It sets MaxRetries=3 and
// succeeds on the third attempt, which both countings reach, so it passes
// either way. These tests count the attempts the client actually makes.

// countingTransport answers every request with the same retryable status and
// counts the calls, so a test can read how many attempts the client made.
func countingTransport(status int, body string, counter *int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*counter++
		return newJSONResponse(status, body), nil
	})}
}

func ocrClient(t *testing.T, maxRetries int, counter *int) *mistral.Client {
	t.Helper()
	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = countingTransport(http.StatusTooManyRequests, "rate limited", counter)
	c.MaxRetries = maxRetries
	// Shrink the backoff so the table below does not sleep through real waits.
	// A zero falls back to the shipped default, so the value must be positive.
	c.InitialBackoff = time.Nanosecond
	c.MaxBackoff = time.Nanosecond
	return c
}

// TestOCRMakesOneAttemptPlusMaxRetries is the contract. MaxRetries counts
// RETRIES, so the total attempt count is always one more than the value.
func TestOCRMakesOneAttemptPlusMaxRetries(t *testing.T) {
	for _, tc := range []struct {
		maxRetries   int
		wantAttempts int
	}{
		{0, 1},  // no retry: one attempt
		{1, 2},  // the value that used to disable OCR retry entirely
		{3, 4},  // the shipped default
		{-1, 1}, // a negative value clamps to no retry
	} {
		attempts := 0
		c := ocrClient(t, tc.maxRetries, &attempts)
		if _, err := c.Extract(context.Background(), "file.pdf", []byte("data")); err == nil {
			t.Fatalf("MaxRetries=%d: expected the retryable failure to surface", tc.maxRetries)
		}
		if attempts != tc.wantAttempts {
			t.Errorf("MaxRetries=%d made %d attempts, want %d (one attempt plus MaxRetries retries)",
				tc.maxRetries, attempts, tc.wantAttempts)
		}
	}
}

// TestOCRRetryCanSucceedOnTheSecondAttempt is the case the old counting broke.
// With MaxRetries=1 the operation must retry once, so a call that fails once
// and then succeeds returns the result rather than the first error.
func TestOCRRetryCanSucceedOnTheSecondAttempt(t *testing.T) {
	attempts := 0
	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.MaxRetries = 1
	c.InitialBackoff = time.Nanosecond
	c.MaxBackoff = time.Nanosecond
	c.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return newJSONResponse(http.StatusTooManyRequests, "rate limited"), nil
		}
		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"ok"}]}`), nil
	})}

	out, err := c.Extract(context.Background(), "file.pdf", []byte("data"))
	if err != nil {
		t.Fatalf("MaxRetries=1 must retry once, but the call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want the second attempt's result", out)
	}
	if attempts != 2 {
		t.Fatalf("made %d attempts, want 2", attempts)
	}
}

// TestOCRAndEmbedAgreeOnTheAttemptCount is the guard against a second drift.
// The two operations share one MaxRetries field, so the same value must buy
// the same number of attempts. This is what #698 reported: one field, two
// meanings, and no test comparing them.
func TestOCRAndEmbedAgreeOnTheAttemptCount(t *testing.T) {
	const maxRetries = 2

	ocrAttempts := 0
	ocr := ocrClient(t, maxRetries, &ocrAttempts)
	_, _ = ocr.Extract(context.Background(), "file.pdf", []byte("data"))

	embedAttempts := 0
	embed := mistral.NewClient("https://api.mistral.ai", "key")
	embed.MaxRetries = maxRetries
	embed.InitialBackoff = time.Nanosecond
	embed.MaxBackoff = time.Nanosecond
	embed.HTTPClient = countingTransport(http.StatusTooManyRequests, "rate limited", &embedAttempts)
	_, _ = embed.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"x"})

	if ocrAttempts != embedAttempts {
		t.Fatalf("one MaxRetries value bought %d OCR attempts and %d embed attempts; "+
			"the field must mean the same thing for both", ocrAttempts, embedAttempts)
	}
}
