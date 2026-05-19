package tests

import (
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func mustNewIngestService(t *testing.T, cfg config.Config, st model.Store) *ingest.Service {
	t.Helper()
	provider := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	// Post clean-break (#38) the Mistral credential resolves from env
	// via the built-in ${MISTRAL_API_KEY} placeholder, not a Config field.
	if provider == "mistral" && strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")) == "" {
		cfg.STTProvider = "off"
	}
	svc, err := ingest.NewService(cfg, st)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	return svc
}
