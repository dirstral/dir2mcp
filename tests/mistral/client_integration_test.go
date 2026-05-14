package tests

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mistral"
)

func TestEmbed_Integration_MistralAPI(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to run integration tests")
	}

	apiKey := strings.TrimSpace(os.Getenv("MISTRAL_API_KEY"))
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}

	baseURL := strings.TrimSpace(os.Getenv("MISTRAL_BASE_URL"))
	client := mistral.NewClient(baseURL, apiKey)
	client.BatchSize = 2
	client.MaxRetries = 2

	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout())
	defer cancel()

	inputs := []string{
		"dir2mcp integration test sentence one",
		"dir2mcp integration test sentence two",
	}

	vectors, err := client.Embed(ctx, "mistral-embed", inputs)
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(vectors) != len(inputs) {
		t.Fatalf("unexpected vector count: got %d want %d", len(vectors), len(inputs))
	}
	firstDim := len(vectors[0])

	for i, vec := range vectors {
		if len(vec) == 0 {
			t.Fatalf("vector %d is empty", i)
		}
		if len(vec) != firstDim {
			t.Fatalf("vector %d has length %d, expected %d", i, len(vec), firstDim)
		}
	}
}

func integrationTestTimeout() time.Duration {
	const defaultTimeout = 30 * time.Second

	// CI can tune this integration timeout via TEST_TIMEOUT_MS (preferred) or TEST_TIMEOUT_SECONDS.
	if timeoutMS := strings.TrimSpace(os.Getenv("TEST_TIMEOUT_MS")); timeoutMS != "" {
		value, err := strconv.Atoi(timeoutMS)
		if err == nil && value > 0 {
			return time.Duration(value) * time.Millisecond
		}
	}

	if timeoutSeconds := strings.TrimSpace(os.Getenv("TEST_TIMEOUT_SECONDS")); timeoutSeconds != "" {
		value, err := strconv.Atoi(timeoutSeconds)
		if err == nil && value > 0 {
			return time.Duration(value) * time.Second
		}
	}

	return defaultTimeout
}
