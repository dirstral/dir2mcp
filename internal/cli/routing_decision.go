package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// printRoutingSection emits the "Models" banner section that explains
// which provider/extractor will serve each capability and why. The
// section is silent when there is nothing useful to show (every row is
// the trivial "unavailable" with no reason). When at least one
// capability has a non-empty reason, the section prints the dimmed
// suffix so users can see why a fallback or unavailable state was
// picked — this is the diagnostic surface that makes
// "OCR: mistral-ocr (fallback; docling not found on PATH)" visible at
// startup time.
func printRoutingSection(out io.Writer, s styles, rows []routingRow) {
	if !routingSectionHasContent(rows) {
		return
	}
	writef(out, "  %s\n", s.sectionHeader("Models"))
	for _, row := range rows {
		value := row.Provider
		if row.Reason != "" {
			value = fmt.Sprintf("%s %s", row.Provider, s.dim("("+row.Reason+")"))
		}
		writeln(out, s.kv(row.Capability, value))
	}
	writeln(out)
}

// routingSectionHasContent reports whether at least one row carries
// useful information (a known provider or any explanatory reason).
func routingSectionHasContent(rows []routingRow) bool {
	for _, row := range rows {
		if row.Provider != "" && row.Provider != "unavailable" {
			return true
		}
		if row.Reason != "" {
			return true
		}
	}
	return false
}

// routingRow describes one capability's resolution for the startup
// banner. Provider is the profile name (or "" if none resolved); Reason
// is a short suffix explaining the choice when not obvious (e.g.
// "fallback; docling not found"). Both are renderable to a single banner
// line. JSON tags match the snake_case convention used elsewhere
// (status, list-files) so the support bundle's routing.json is
// consistent with other tooling.
type routingRow struct {
	Capability string `json:"capability"`
	Provider   string `json:"provider"`
	Reason     string `json:"reason,omitempty"`
}

// routingDecisions summarizes the providers that will serve the active
// capabilities, plus the document-extractor choice (which is not a
// provider). Returned in the order embed -> chat -> OCR so the banner
// reads top-to-bottom by index dependency. Only capabilities that have
// any meaningful state (configured, fallback, or explicitly disabled
// when relevant) are included.
func routingDecisions(cfg config.Config) []routingRow {
	rows := []routingRow{
		providerRow(cfg, "Embed", provider.CapEmbed),
		providerRow(cfg, "Chat", provider.CapChat),
	}
	rows = append(rows, extractorRow(cfg))
	return rows
}

// providerRow resolves cap through the provider model and renders a
// routing row. A *provider.ConfigError is surfaced verbatim because it
// already explains *why* selection failed (bad explicit binding,
// incapable kind, etc.).
func providerRow(cfg config.Config, label string, cap provider.Capability) routingRow {
	prof, err := cfg.Providers().Resolve(cap)
	if err != nil {
		var ce *provider.ConfigError
		if errors.As(err, &ce) {
			return routingRow{Capability: label, Provider: "unavailable", Reason: ce.Error()}
		}
		return routingRow{Capability: label, Provider: "unavailable", Reason: err.Error()}
	}
	return routingRow{Capability: label, Provider: prof.Name}
}

// extractorRow renders the document-extractor decision. It uses
// ingest.DescribeDocumentExtractor so the banner reflects the same
// selection the runtime will perform. The Source ("auto", "fallback",
// etc.) is folded into the reason since the reason text already
// conveys whichever distinction matters.
func extractorRow(cfg config.Config) routingRow {
	d := ingest.DescribeDocumentExtractor(cfg)
	name := d.Name
	if name == "" {
		name = "disabled"
	}
	return routingRow{Capability: "OCR", Provider: name, Reason: d.Reason}
}
