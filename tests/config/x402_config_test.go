package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

func TestLoad_EnvOverridesX402(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_X402_MODE", "on")
		t.Setenv("DIR2MCP_X402_FACILITATOR_URL", "https://facilitator.example.com")
		t.Setenv("DIR2MCP_X402_RESOURCE_BASE_URL", "https://resource.example.com")
		t.Setenv("DIR2MCP_X402_NETWORK", "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")
		t.Setenv("DIR2MCP_X402_ASSET", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		t.Setenv("DIR2MCP_X402_PAY_TO", "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj")

		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if cfg.X402.Mode != "on" {
			t.Fatalf("X402.Mode=%q want=%q", cfg.X402.Mode, "on")
		}
		if cfg.X402.FacilitatorURL != "https://facilitator.example.com" {
			t.Fatalf("X402.FacilitatorURL=%q want=%q", cfg.X402.FacilitatorURL, "https://facilitator.example.com")
		}
		if cfg.X402.ResourceBaseURL != "https://resource.example.com" {
			t.Fatalf("X402.ResourceBaseURL=%q want=%q", cfg.X402.ResourceBaseURL, "https://resource.example.com")
		}
		if cfg.X402.Network != "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
			t.Fatalf("X402.Network=%q want=%q", cfg.X402.Network, "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")
		}
		if cfg.X402.Asset != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" {
			t.Fatalf("X402.Asset=%q want=%q", cfg.X402.Asset, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		}
		if cfg.X402.PayTo != "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj" {
			t.Fatalf("X402.PayTo=%q want=%q", cfg.X402.PayTo, "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj")
		}
	})
}

func TestLoad_EnvOverridesX402_EmptyIgnored(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		// start with a config file specifying non-empty values
		yaml := `x402_facilitator_url: https://facilitator.example.com
x402_resource_base_url: https://resource.example.com
` // token is sensitive and ignored by config file
		writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), yaml)

		// set environment variables to empty or whitespace
		t.Setenv("DIR2MCP_X402_FACILITATOR_URL", "")
		t.Setenv("DIR2MCP_X402_FACILITATOR_TOKEN", "  ")
		t.Setenv("DIR2MCP_X402_RESOURCE_BASE_URL", "   ")

		// load config file so that file values are applied
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		// blank env values should not override config file values (or default for token)
		if cfg.X402.FacilitatorURL != "https://facilitator.example.com" {
			t.Fatalf("expected facilitator URL to remain unchanged, got %q", cfg.X402.FacilitatorURL)
		}
		if cfg.X402.ResourceBaseURL != "https://resource.example.com" {
			t.Fatalf("expected resource base URL to remain unchanged, got %q", cfg.X402.ResourceBaseURL)
		}
		if cfg.X402.FacilitatorToken != "" {
			t.Fatalf("expected facilitator token to stay empty, got %q", cfg.X402.FacilitatorToken)
		}
	})
}

func TestValidateX402_ModeWithToolsDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.X402.Mode = "required" // mode says we need X402
	cfg.X402.ToolsCallEnabled = false
	// other fields irrelevant because validation should fail early
	err := cfg.ValidateX402(true)
	if err == nil {
		t.Fatal("expected validation error when tools call disabled but mode is not off")
	}
	if !strings.Contains(err.Error(), "ToolsCallEnabled") {
		t.Fatalf("error message did not mention ToolsCallEnabled: %v", err)
	}
}

func TestValidateX402_NormalizesMode(t *testing.T) {
	cfg := config.Default()
	cfg.X402.Mode = "  On  "
	cfg.X402.ToolsCallEnabled = true
	if err := cfg.ValidateX402(false); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.X402.Mode != "on" {
		t.Fatalf("mode not normalized, got %q", cfg.X402.Mode)
	}

	// ensure an empty value is defaulted to off
	cfg = config.Default()
	cfg.X402.Mode = ""
	cfg.X402.ToolsCallEnabled = true
	if err := cfg.ValidateX402(false); err != nil {
		t.Fatalf("unexpected validation error with blank mode: %v", err)
	}
	if cfg.X402.Mode != "off" {
		t.Fatalf("blank mode should default to off, got %q", cfg.X402.Mode)
	}
}

// trailing slashes in the configured URLs should be stripped by validation
func TestValidateX402_StripsTrailingSlashes(t *testing.T) {
	cfg := config.Default()
	cfg.X402.Mode = "required"
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = "https://facilitator.example.com/"
	cfg.X402.ResourceBaseURL = "https://resource.example.com//"
	cfg.X402.FacilitatorToken = "token"
	cfg.X402.PriceAtomic = "100"
	cfg.X402.Scheme = "exact"
	cfg.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	cfg.X402.Asset = "asset"
	cfg.X402.PayTo = "payto"

	if err := cfg.ValidateX402(false); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.X402.FacilitatorURL != "https://facilitator.example.com" {
		t.Fatalf("facilitator URL not normalized, got %q", cfg.X402.FacilitatorURL)
	}
	if cfg.X402.ResourceBaseURL != "https://resource.example.com" {
		t.Fatalf("resource base URL not normalized, got %q", cfg.X402.ResourceBaseURL)
	}
}

func TestValidateX402_InvalidNetwork(t *testing.T) {
	cfg := config.Default()
	cfg.X402.Mode = "required" // enable validation path
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = "https://facilitator.example.com"
	cfg.X402.ResourceBaseURL = "https://resource.example.com"
	cfg.X402.FacilitatorToken = "token"
	cfg.X402.PriceAtomic = "1000"
	cfg.X402.Scheme = "exact"
	cfg.X402.Network = "not-a-caip2-network"
	cfg.X402.Asset = "asset"
	cfg.X402.PayTo = "payto"

	err := cfg.ValidateX402(true)
	if err == nil {
		t.Fatal("expected strict x402 validation error for invalid network")
	}
	if !strings.Contains(err.Error(), "CAIP-2") {
		t.Fatalf("expected CAIP-2 validation error, got: %v", err)
	}
}

func TestValidateX402_PriceMustBePositiveInteger(t *testing.T) {
	// helper to build a fresh valid config with required fields set;
	// subtests will add the price value they want to check.
	makeBase := func() *config.Config {
		c := config.Default()
		// take address so callers can mutate without copying
		cp := &c
		cp.X402.Mode = "required"
		cp.X402.ToolsCallEnabled = true
		cp.X402.FacilitatorURL = "https://facilitator.example.com"
		cp.X402.ResourceBaseURL = "https://resource.example.com"
		cp.X402.FacilitatorToken = "token"
		cp.X402.Scheme = "exact"
		cp.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" // valid CAIP-2
		cp.X402.Asset = "asset"
		cp.X402.PayTo = "payto"
		return cp
	}

	// iterate through a few invalid price strings, running each in a subtest
	for _, tc := range []struct {
		name string
		bad  string
	}{
		{"empty", ""},
		{"non-integer", "abc"},
		{"negative", "-100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localCfg := makeBase()
			localCfg.X402.PriceAtomic = tc.bad
			err := localCfg.ValidateX402(true)
			if err == nil {
				t.Fatalf("price %q should have failed validation", tc.bad)
			}
			if tc.bad == "" {
				if !strings.Contains(err.Error(), "required") {
					t.Fatalf("empty price produced wrong error: %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), "positive integer") {
					t.Fatalf("unexpected error message for price %q: %v", tc.bad, err)
				}
			}
		})
	}

	// additional explicit price checks in subtests for consistent reporting
	t.Run("price=0 rejects", func(t *testing.T) {
		local := makeBase()
		local.X402.PriceAtomic = "0"
		if err := local.ValidateX402(true); err == nil {
			t.Fatal("expected price 0 to be rejected")
		}
	})
	t.Run("price=12345 accepts", func(t *testing.T) {
		local := makeBase()
		local.X402.PriceAtomic = "12345"
		if err := local.ValidateX402(true); err != nil {
			t.Fatalf("expected positive price to be valid, got %v", err)
		}
	})
}

func TestValidateX402_InvalidScheme(t *testing.T) {
	cfg := config.Default()
	cfg.X402.Mode = "required"
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = "https://facilitator.example.com"
	cfg.X402.ResourceBaseURL = "https://resource.example.com"
	cfg.X402.FacilitatorToken = "token"
	cfg.X402.PriceAtomic = "1000"
	cfg.X402.Scheme = "not-allowed"
	cfg.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	cfg.X402.Asset = "asset"
	cfg.X402.PayTo = "payto"

	err := cfg.ValidateX402(true)
	if err == nil {
		t.Fatal("expected strict x402 validation error for invalid scheme")
	}
	if !strings.Contains(err.Error(), "one of") {
		t.Fatalf("unexpected error message for scheme: %v", err)
	}

	// subsequent checks should not mutate the original configuration; make
	// copies so each assertion is independent
	copyCfg := cfg
	copyCfg.X402.Scheme = "EXACT"
	if err := copyCfg.ValidateX402(true); err != nil {
		t.Fatalf("expected uppercase exact to be accepted, got %v", err)
	}

	copyCfg = cfg
	copyCfg.X402.Scheme = " UpTo "
	if err := copyCfg.ValidateX402(true); err != nil {
		t.Fatalf("expected spaced/upper upto to be accepted, got %v", err)
	}
}
