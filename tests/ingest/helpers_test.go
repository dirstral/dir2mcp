package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func mustNewIngestService(t *testing.T, cfg config.Config, st model.Store) *ingest.Service {
	t.Helper()
	provider := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	if provider == "mistral" && strings.TrimSpace(cfg.MistralAPIKey) == "" {
		cfg.STTProvider = "off"
	}
	svc, err := ingest.NewService(cfg, st)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	return svc
}
