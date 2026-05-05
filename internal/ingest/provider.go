package ingest

import (
	"os/exec"
	"strings"

	"dir2mcp/internal/config"
)

const (
	IngestProviderAuto    = "auto"
	IngestProviderNative  = "native"
	IngestProviderDocling = "docling"
)

type ProviderSelection struct {
	Configured     string `json:"configured"`
	Selected       string `json:"selected"`
	DoclingCommand string `json:"docling_command,omitempty"`
	DoclingFound   bool   `json:"docling_found"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

func ResolveProvider(cfg config.Config) ProviderSelection {
	configured := strings.ToLower(strings.TrimSpace(cfg.IngestProvider))
	if configured == "" {
		configured = IngestProviderAuto
	}

	command := strings.TrimSpace(cfg.IngestDoclingCommand)
	if command == "" {
		command = config.Default().IngestDoclingCommand
	}

	_, lookErr := exec.LookPath(command)
	doclingFound := lookErr == nil

	switch configured {
	case IngestProviderDocling:
		if doclingFound {
			return ProviderSelection{
				Configured:     configured,
				Selected:       IngestProviderDocling,
				DoclingCommand: command,
				DoclingFound:   true,
			}
		}
		return ProviderSelection{
			Configured:     configured,
			Selected:       IngestProviderNative,
			DoclingCommand: command,
			DoclingFound:   false,
			FallbackReason: "docling command not found",
		}
	case IngestProviderNative:
		return ProviderSelection{
			Configured: configured,
			Selected:   IngestProviderNative,
		}
	default:
		if doclingFound {
			return ProviderSelection{
				Configured:     IngestProviderAuto,
				Selected:       IngestProviderDocling,
				DoclingCommand: command,
				DoclingFound:   true,
			}
		}
		return ProviderSelection{
			Configured:     IngestProviderAuto,
			Selected:       IngestProviderNative,
			DoclingCommand: command,
			DoclingFound:   false,
			FallbackReason: "docling unavailable in auto mode",
		}
	}
}
