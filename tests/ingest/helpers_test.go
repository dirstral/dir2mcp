package tests

import (
	"strings"
	"testing"

	"dir2mcp/internal/config"
	"dir2mcp/internal/ingest"
	"dir2mcp/internal/model"
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
