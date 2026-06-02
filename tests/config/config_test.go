package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// resolvedEmbedAPIKey loads via config.Load (which applies dotenv into
// the process env) and returns the credential the provider resolver
// expands for the built-in mistral profile's ${MISTRAL_API_KEY}
// placeholder. Post clean-break (#38) the Mistral credential is no
// longer a Config field — dotenv/env feeds the resolver, not Config.
func resolvedEmbedAPIKey(t *testing.T, tmp string) string {
	t.Helper()
	var key string
	testutil.WithWorkingDir(t, tmp, func() {
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		p, err := cfg.Providers().Resolve(provider.CapEmbed)
		if err != nil {
			t.Fatalf("resolve embed: %v", err)
		}
		key = p.APIKey
	})
	return key
}

func TestLoad_UsesDotEnvWhenEnvIsMissing(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "MISTRAL_API_KEY=from_dotenv\n")

	t.Setenv("MISTRAL_API_KEY", "")
	if got := resolvedEmbedAPIKey(t, tmp); got != "from_dotenv" {
		t.Fatalf("unexpected resolved api key: %q", got)
	}
}

func TestLoad_EnvOverridesDotEnv(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "MISTRAL_API_KEY=from_dotenv\n")

	t.Setenv("MISTRAL_API_KEY", "from_env")
	if got := resolvedEmbedAPIKey(t, tmp); got != "from_env" {
		t.Fatalf("unexpected resolved api key: %q", got)
	}
}

func TestConfigValidateRejectsInvalidIngestExtractor(t *testing.T) {
	cfg := config.Default()
	cfg.IngestExtractor = "invalid-mode"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for ingest extractor mode")
	}
}

func TestConfigValidateAcceptsDoclingServeExtractor(t *testing.T) {
	cfg := config.Default()
	cfg.IngestExtractor = "docling-serve"
	cfg.IngestDoclingServeURL = "http://127.0.0.1:5001"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("docling-serve must be a valid extractor: %v", err)
	}
}

func TestLoad_DotEnvLocalOverridesDotEnv(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "MISTRAL_API_KEY=from_env_file\n")
	writeFile(t, filepath.Join(tmp, ".env.local"), "MISTRAL_API_KEY=from_env_local\n")

	t.Setenv("MISTRAL_API_KEY", "")
	if got := resolvedEmbedAPIKey(t, tmp); got != "from_env_local" {
		t.Fatalf("unexpected resolved api key: %q", got)
	}
}

func TestLoad_UsesDotEnvWhenEnvIsMissing_ElevenLabs(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "ELEVENLABS_API_KEY=el_from_dotenv\nELEVENLABS_BASE_URL=https://el-dotenv.local\nELEVENLABS_VOICE_ID=voice-from-dotenv\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("ELEVENLABS_API_KEY", "")
		t.Setenv("ELEVENLABS_BASE_URL", "")
		t.Setenv("ELEVENLABS_VOICE_ID", "")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.ElevenLabsAPIKey != "el_from_dotenv" {
			t.Fatalf("unexpected elevenlabs api key: %q", cfg.ElevenLabsAPIKey)
		}
		if cfg.ElevenLabsBaseURL != "https://el-dotenv.local" {
			t.Fatalf("unexpected elevenlabs base URL: %q", cfg.ElevenLabsBaseURL)
		}
		if cfg.ElevenLabsTTSVoiceID != "voice-from-dotenv" {
			t.Fatalf("unexpected elevenlabs voice id: %q", cfg.ElevenLabsTTSVoiceID)
		}
	})
}

func TestLoad_EnvOverridesDotEnv_ElevenLabs(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "ELEVENLABS_API_KEY=el_from_dotenv\nELEVENLABS_BASE_URL=https://el-dotenv.local\nELEVENLABS_VOICE_ID=voice-from-dotenv\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("ELEVENLABS_API_KEY", "el_from_env")
		t.Setenv("ELEVENLABS_BASE_URL", "https://el-env.local")
		t.Setenv("ELEVENLABS_VOICE_ID", "voice-from-env")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.ElevenLabsAPIKey != "el_from_env" {
			t.Fatalf("unexpected elevenlabs api key: %q", cfg.ElevenLabsAPIKey)
		}
		if cfg.ElevenLabsBaseURL != "https://el-env.local" {
			t.Fatalf("unexpected elevenlabs base URL: %q", cfg.ElevenLabsBaseURL)
		}
		if cfg.ElevenLabsTTSVoiceID != "voice-from-env" {
			t.Fatalf("unexpected elevenlabs voice id: %q", cfg.ElevenLabsTTSVoiceID)
		}
	})
}

func TestLoad_DotEnvLocalOverridesDotEnv_ElevenLabs(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".env"), "ELEVENLABS_API_KEY=el_env_file\nELEVENLABS_BASE_URL=https://el-env-file.local\nELEVENLABS_VOICE_ID=voice-env-file\n")
	writeFile(t, filepath.Join(tmp, ".env.local"), "ELEVENLABS_API_KEY=el_env_local\nELEVENLABS_BASE_URL=https://el-env-local.local\nELEVENLABS_VOICE_ID=voice-env-local\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("ELEVENLABS_API_KEY", "")
		t.Setenv("ELEVENLABS_BASE_URL", "")
		t.Setenv("ELEVENLABS_VOICE_ID", "")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.ElevenLabsAPIKey != "el_env_local" {
			t.Fatalf("unexpected elevenlabs api key: %q", cfg.ElevenLabsAPIKey)
		}
		if cfg.ElevenLabsBaseURL != "https://el-env-local.local" {
			t.Fatalf("unexpected elevenlabs base URL: %q", cfg.ElevenLabsBaseURL)
		}
		if cfg.ElevenLabsTTSVoiceID != "voice-env-local" {
			t.Fatalf("unexpected elevenlabs voice id: %q", cfg.ElevenLabsTTSVoiceID)
		}
	})
}

func TestLoad_DefaultElevenLabsVoiceID(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("ELEVENLABS_VOICE_ID", "")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.ElevenLabsTTSVoiceID != "JBFqnCBsd6RMkjVDRZzb" {
			t.Fatalf("unexpected default elevenlabs voice id: %q", cfg.ElevenLabsTTSVoiceID)
		}
	})
}

func TestLoad_SessionTimeout_EnvAndYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "session_inactivity_timeout: 1s\nsession_max_lifetime: 2s\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.SessionInactivityTimeout != time.Second {
		t.Fatalf("unexpected inactivity timeout: %v", cfg.SessionInactivityTimeout)
	}
	if cfg.SessionMaxLifetime != 2*time.Second {
		t.Fatalf("unexpected max lifetime: %v", cfg.SessionMaxLifetime)
	}

	testutil.WithWorkingDir(t, tmp, func() {
		// old variable name should still work
		t.Setenv("DIR2MCP_SESSION_TIMEOUT", "3s")
		t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", "4s")
		cfg2, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg2.SessionInactivityTimeout != 3*time.Second {
			t.Fatalf("env override inactivity timeout=%v want=3s", cfg2.SessionInactivityTimeout)
		}
		if cfg2.SessionMaxLifetime != 4*time.Second {
			t.Fatalf("env override max lifetime=%v want=4s", cfg2.SessionMaxLifetime)
		}

		// now prefer new variable name when both are present
		t.Setenv("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", "5s")
		// keep max lifetime >= inactivity timeout for validation
		t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", "6s")
		cfg3, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg3.SessionInactivityTimeout != 5*time.Second {
			t.Fatalf("prefer new env var; got %v want=5s", cfg3.SessionInactivityTimeout)
		}
		if cfg3.SessionMaxLifetime != 6*time.Second {
			t.Fatalf("env max lifetime=%v want=6s", cfg3.SessionMaxLifetime)
		}
	})
}

func TestLoad_SessionTimeout_EnvWhitespace(t *testing.T) {
	tmp := t.TempDir()
	testutil.WithWorkingDir(t, tmp, func() {
		// two related scenarios are exercised via subtests so that the
		// outer t.Setenv cleanups stay in place and we never manually
		// call os.Unsetenv.
		t.Run("trimmed values", func(t *testing.T) {
			// whitespace around duration should be trimmed before parsing
			t.Setenv("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", " 7s ")
			t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", " 8s ")
			cfg, err := config.Load("")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.SessionInactivityTimeout != 7*time.Second {
				t.Fatalf("expected 7s after trimming whitespace; got %v", cfg.SessionInactivityTimeout)
			}
			if cfg.SessionMaxLifetime != 8*time.Second {
				t.Fatalf("expected 8s after trimming whitespace; got %v", cfg.SessionMaxLifetime)
			}
		})

		t.Run("invalid trimmed DIR2MCP_SESSION_INACTIVITY_TIMEOUT warns mentioning trimmed value", func(t *testing.T) {
			// invalid value with whitespace should produce a warning mentioning the trimmed
			// clear max lifetime so default inactivity fallback does not violate
			// max>=inactivity validation when inactivity parsing fails.
			t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", "")
			t.Setenv("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", "  notaduration  ")
			cfg2, err := config.Load("")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if len(cfg2.Warnings) == 0 {
				t.Fatal("expected warning for invalid duration")
			}
			found := false
			for _, w := range cfg2.Warnings {
				if strings.Contains(w.Error(), "notaduration") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("warning did not mention trimmed value: %v", cfg2.Warnings)
			}
		})
	})
}

func TestLoad_SessionDurations_Validation(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	t.Run("negative inactivity YAML", func(t *testing.T) { sessionLoadFileExpectError(t, path, "session_inactivity_timeout: -1s\n") })
	t.Run("zero inactivity defaults YAML", func(t *testing.T) { subtestZeroInactivityDefaults(t, path) })
	t.Run("negative max lifetime YAML", func(t *testing.T) { sessionLoadFileExpectError(t, path, "session_max_lifetime: -1s\n") })
	t.Run("zero max lifetime defaults YAML", func(t *testing.T) { subtestZeroMaxLifetimeDefaults(t, path) })
	t.Run("env negative inactivity", func(t *testing.T) { subtestEnvLoadExpectError(t, tmp, "DIR2MCP_SESSION_INACTIVITY_TIMEOUT", "-5s") })
	t.Run("env negative max lifetime", func(t *testing.T) { subtestEnvLoadExpectError(t, tmp, "DIR2MCP_SESSION_MAX_LIFETIME", "-5s") })
	t.Run("health_check interval YAML and env override", func(t *testing.T) { subtestHealthCheckYAMLAndEnvOverride(t, tmp, path) })
	t.Run("negative health YAML", func(t *testing.T) { sessionLoadFileExpectError(t, path, "health_check_interval: -1s\n") })
	t.Run("negative health env", func(t *testing.T) { subtestEnvLoadExpectError(t, tmp, "DIR2MCP_HEALTH_CHECK_INTERVAL", "-5s") })
	t.Run("max lifetime < inactivity YAML", func(t *testing.T) {
		sessionLoadFileExpectError(t, path, "session_inactivity_timeout: 10s\nsession_max_lifetime: 5s\n")
	})
	t.Run("env max lifetime < inactivity", func(t *testing.T) { subtestEnvMaxLifetimeLessThanInactivity(t, tmp) })
}

func sessionLoadFileExpectError(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body)
	if _, err := config.LoadFile(path); err == nil {
		t.Fatalf("expected error loading config: %s", body)
	}
}

func subtestZeroInactivityDefaults(t *testing.T, path string) {
	writeFile(t, path, "session_inactivity_timeout: 0s\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cfg.SessionInactivityTimeout != 24*time.Hour {
		t.Fatalf("zero inactivity timeout did not default, got %v", cfg.SessionInactivityTimeout)
	}
}

func subtestZeroMaxLifetimeDefaults(t *testing.T, path string) {
	writeFile(t, path, "session_max_lifetime: 0s\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cfg.SessionMaxLifetime != config.Default().SessionMaxLifetime {
		t.Fatalf("zero max lifetime did not default, got %v want %v", cfg.SessionMaxLifetime, config.Default().SessionMaxLifetime)
	}
}

func subtestEnvLoadExpectError(t *testing.T, tmp, envKey, envVal string) {
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv(envKey, envVal)
		if _, err := config.Load(""); err == nil {
			t.Fatalf("expected error from env %s=%s", envKey, envVal)
		}
	})
}

func subtestHealthCheckYAMLAndEnvOverride(t *testing.T, tmp, path string) {
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", "")
		t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", "")
		writeFile(t, path, "health_check_interval: 10s\n")
		cfg, err := config.LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile failed: %v", err)
		}
		if cfg.HealthCheckInterval != 10*time.Second {
			t.Fatalf("unexpected health interval from YAML: %v", cfg.HealthCheckInterval)
		}

		t.Setenv("DIR2MCP_HEALTH_CHECK_INTERVAL", "15s")
		cfg, err = config.Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.HealthCheckInterval != 15*time.Second {
			t.Fatalf("env override health interval=%v want=15s", cfg.HealthCheckInterval)
		}
	})
}

func subtestEnvMaxLifetimeLessThanInactivity(t *testing.T, tmp string) {
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_SESSION_INACTIVITY_TIMEOUT", "10s")
		t.Setenv("DIR2MCP_SESSION_MAX_LIFETIME", "5s")
		if _, err := config.Load(""); err == nil {
			t.Fatalf("expected error for env max lifetime < inactivity")
		}
	})
}

func TestLoad_X402FacilitatorTokenEnvOnly(t *testing.T) {
	// split into explicit subtests for clarity
	// each subtest gets its own temporary working directory and environment

	t.Run("file-only", func(t *testing.T) {
		tmp := t.TempDir()
		testutil.WithWorkingDir(t, tmp, func() {
			// write a config file containing the sensitive field; it should be ignored
			writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "x402_facilitator_token: should-not-be-used\n")
			// ensure the env var is blank so loader falls back to default
			t.Setenv("DIR2MCP_X402_FACILITATOR_TOKEN", "")
			cfg, err := config.Load("")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.X402.FacilitatorToken != "" {
				t.Fatalf("expected empty token when config file provides it, got %q", cfg.X402.FacilitatorToken)
			}
		})
	})

	t.Run("env-override", func(t *testing.T) {
		tmp := t.TempDir()
		testutil.WithWorkingDir(t, tmp, func() {
			// config file still contains a value that should be ignored
			writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "x402_facilitator_token: should-not-be-used\n")
			// set the actual override
			t.Setenv("DIR2MCP_X402_FACILITATOR_TOKEN", "envval")
			cfg, err := config.Load("")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.X402.FacilitatorToken != "envval" {
				t.Fatalf("expected envval, got %q", cfg.X402.FacilitatorToken)
			}
		})
	})

	t.Run("env-cleared", func(t *testing.T) {
		tmp := t.TempDir()
		testutil.WithWorkingDir(t, tmp, func() {
			// config file again contains a token that should be ignored
			writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "x402_facilitator_token: should-not-be-used\n")
			// simulate a previously-set environment variable, then remove it.
			// we call Setenv first so the testing harness tracks the variable, but
			// immediately Unsetenv to ensure config.Load("") sees no value.  The
			// subtest is named "env-cleared" and below we assert that
			// cfg.X402.FacilitatorToken ends up empty once the env var is removed.
			t.Setenv("DIR2MCP_X402_FACILITATOR_TOKEN", "envval")
			// clearing should be done with Unsetenv so Load() sees no value
			if err := os.Unsetenv("DIR2MCP_X402_FACILITATOR_TOKEN"); err != nil {
				t.Fatalf("Unsetenv failed: %v", err)
			}
			cfg, err := config.Load("")
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.X402.FacilitatorToken != "" {
				t.Fatalf("expected token to be empty after env cleared, got %q", cfg.X402.FacilitatorToken)
			}
		})
	})
}

func TestLoad_InvalidX402ToolsCallEnabledEnvWarning(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_X402_TOOLS_CALL_ENABLED", "notabool")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// default should remain true
		if !cfg.X402.ToolsCallEnabled {
			t.Fatalf("expected default ToolsCallEnabled=true, got %v", cfg.X402.ToolsCallEnabled)
		}
		if len(cfg.Warnings) == 0 {
			t.Fatal("expected at least one warning")
		}
		found := false
		for _, w := range cfg.Warnings {
			if strings.Contains(w.Error(), "DIR2MCP_X402_TOOLS_CALL_ENABLED") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("warning list did not contain expected message: %v", cfg.Warnings)
		}
	})
}

func TestLoad_AllowedOriginsEnvAppendsToDefaults(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "https://elevenlabs.io,https://my-app.example.com")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertContains(t, cfg.AllowedOrigins, "http://localhost")
		assertContains(t, cfg.AllowedOrigins, "http://127.0.0.1")
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
		assertContains(t, cfg.AllowedOrigins, "https://my-app.example.com")
	})
}

func TestLoad_AllowedOriginsEnvDeduplicatesHostCase(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "HTTP://LOCALHOST,https://elevenlabs.io")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		localhostCount := 0
		for _, origin := range cfg.AllowedOrigins {
			if origin == "http://localhost" || origin == "HTTP://LOCALHOST" {
				localhostCount++
			}
		}
		if localhostCount != 1 {
			t.Fatalf("expected exactly one localhost origin entry, got %d (%v)", localhostCount, cfg.AllowedOrigins)
		}
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
	})
}

func TestLoad_AllowedOriginsEnvSkipsMalformedOrigins(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "://bad-origin,https://elevenlabs.io")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertNotContains(t, cfg.AllowedOrigins, "://bad-origin")
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
		assertContains(t, cfg.AllowedOrigins, "http://localhost")
	})
}

func TestLoad_AllowedOriginsEnvSkipsPathLikeToken(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "bad/path,https://elevenlabs.io")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertNotContains(t, cfg.AllowedOrigins, "bad/path")
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
	})
}

func TestLoad_AllowedOriginsEnvSkipsBackslashToken(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "bad\\path,https://elevenlabs.io")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertNotContains(t, cfg.AllowedOrigins, "bad\\path")
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
	})
}

func TestLoad_AllowedOriginsEnvSkipsWhitespaceToken(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "bad origin,https://elevenlabs.io")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertNotContains(t, cfg.AllowedOrigins, "bad origin")
		assertContains(t, cfg.AllowedOrigins, "https://elevenlabs.io")
	})
}

func TestLoad_AllowedOriginsEnvDeduplicatesHTTPSDefaultPort(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "https://example.com,https://example.com:443")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		count := 0
		for _, origin := range cfg.AllowedOrigins {
			if origin == "https://example.com" || origin == "https://example.com:443" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected one normalized https example.com entry, got %d (%v)", count, cfg.AllowedOrigins)
		}
		assertContains(t, cfg.AllowedOrigins, "https://example.com")
	})
}

func TestLoad_AllowedOriginsEnvDeduplicatesHTTPDefaultPort(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "http://example.com,http://example.com:80")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		count := 0
		for _, origin := range cfg.AllowedOrigins {
			if origin == "http://example.com" || origin == "http://example.com:80" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected one normalized http example.com entry, got %d (%v)", count, cfg.AllowedOrigins)
		}
		assertContains(t, cfg.AllowedOrigins, "http://example.com")
	})
}

func TestLoad_AllowedOriginsEnvKeepsNonDefaultPortDistinct(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_ALLOWED_ORIGINS", "https://example.com,https://example.com:444")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertContains(t, cfg.AllowedOrigins, "https://example.com")
		assertContains(t, cfg.AllowedOrigins, "https://example.com:444")
	})
}

func TestDefault_RateLimitValues(t *testing.T) {
	cfg := config.Default()
	if cfg.RateLimitRPS != 60 {
		t.Fatalf("RateLimitRPS=%d want=%d", cfg.RateLimitRPS, 60)
	}
	if cfg.RateLimitBurst != 20 {
		t.Fatalf("RateLimitBurst=%d want=%d", cfg.RateLimitBurst, 20)
	}
}

// Post clean-break (#38) the embed model names are profile config
// (built-in mistral defaults; overridable via model.embed.* per SPEC
// §16.2), not Config fields. Resolve through the provider resolver.
func TestDefault_EmbedModels(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "k")
	p, err := loadCfg(t, "version: 1\n").Providers().Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	if p.EmbedTextModel != "mistral-embed" {
		t.Fatalf("unexpected default text embed model: %q", p.EmbedTextModel)
	}
	if p.EmbedCodeModel != "codestral-embed" {
		t.Fatalf("unexpected default code embed model: %q", p.EmbedCodeModel)
	}
}

func TestModel_OverridesEmbedModels(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "k")
	yaml := "model:\n  embed:\n    text_model: foo-model\n    code_model: bar-model\n"
	p, err := loadCfg(t, yaml).Providers().Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	if p.EmbedTextModel != "foo-model" {
		t.Fatalf("unexpected embed model text: %q", p.EmbedTextModel)
	}
	if p.EmbedCodeModel != "bar-model" {
		t.Fatalf("unexpected embed model code: %q", p.EmbedCodeModel)
	}
}

func TestDefault_ChatModel(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "k")
	p, err := loadCfg(t, "version: 1\n").Providers().Resolve(provider.CapChat)
	if err != nil {
		t.Fatalf("resolve chat: %v", err)
	}
	if p.ChatModel != "mistral-small-2506" {
		t.Fatalf("unexpected default chat model: %q", p.ChatModel)
	}
}

func TestDefault_NestedConfigFieldDefaults(t *testing.T) {
	cfg := config.Default()
	assertDefaultRAG(t, cfg)
	assertDefaultIngest(t, cfg)
	assertDefaultSTT(t, cfg)
	assertDefaultTLS(t, cfg)
}

func assertDefaultRAG(t *testing.T, cfg config.Config) {
	t.Helper()
	if cfg.RAGMaxContextChars != 20000 || cfg.RAGOversampleFactor != 5 {
		t.Fatalf("unexpected rag defaults: max=%d oversample=%d", cfg.RAGMaxContextChars, cfg.RAGOversampleFactor)
	}
}

func assertDefaultIngest(t *testing.T, cfg config.Config) {
	t.Helper()
	if !cfg.IngestGitignore || cfg.IngestFollowSymlinks || cfg.IngestMaxFileMB != 20 {
		t.Fatalf("unexpected ingest defaults: gitignore=%v follow=%v max=%d", cfg.IngestGitignore, cfg.IngestFollowSymlinks, cfg.IngestMaxFileMB)
	}
	if cfg.IngestPDFMode != "ocr" || cfg.IngestImagesMode != "ocr_auto" || cfg.IngestAudioMode != "auto" || cfg.IngestArchivesMode != "deep" {
		t.Fatalf("unexpected ingest mode defaults: pdf=%q images=%q audio=%q archives=%q", cfg.IngestPDFMode, cfg.IngestImagesMode, cfg.IngestAudioMode, cfg.IngestArchivesMode)
	}
}

func assertDefaultSTT(t *testing.T, cfg config.Config) {
	t.Helper()
	if cfg.STTProvider != "mistral" || cfg.STTMistralModel == "" || cfg.STTElevenLabsModel == "" {
		t.Fatalf("unexpected stt defaults: provider=%q mistral=%q eleven=%q", cfg.STTProvider, cfg.STTMistralModel, cfg.STTElevenLabsModel)
	}
	if cfg.STTElevenLabsModel != "scribe_v1" {
		t.Fatalf("unexpected elevenlabs stt default model: %q", cfg.STTElevenLabsModel)
	}
}

func assertDefaultTLS(t *testing.T, cfg config.Config) {
	t.Helper()
	if cfg.ServerTLSCertFile != "" || cfg.ServerTLSKeyFile != "" {
		t.Fatalf("unexpected tls defaults: cert=%q key=%q", cfg.ServerTLSCertFile, cfg.ServerTLSKeyFile)
	}
}

func TestLoad_RejectsNegativeRAGAndChunkingTunables(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "negative rag k default",
			yaml: "rag_k_default: -1\n",
			want: "rag.k_default must be non-negative",
		},
		{
			name: "negative chunking max tokens",
			yaml: "chunking_max_tokens: -5\n",
			want: "chunking.max_tokens must be non-negative",
		},
		{
			name: "negative chunking overlap tokens",
			yaml: "chunking_overlap_tokens: -3\n",
			want: "chunking.overlap_tokens must be non-negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, path, tc.yaml)
			_, err := config.LoadFile(path)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error mismatch: got=%v want substring=%q", err, tc.want)
			}
		})
	}
}

func TestModel_OverridesChatModel(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "k")
	yaml := "model:\n  chat:\n    model: new-chat\n"
	p, err := loadCfg(t, yaml).Providers().Resolve(provider.CapChat)
	if err != nil {
		t.Fatalf("resolve chat: %v", err)
	}
	if p.ChatModel != "new-chat" {
		t.Fatalf("unexpected chat model: %q", p.ChatModel)
	}
}

func TestLoad_RateLimitEnvOverrides(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_RATE_LIMIT_RPS", "75")
		t.Setenv("DIR2MCP_RATE_LIMIT_BURST", "25")

		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if cfg.RateLimitRPS != 75 {
			t.Fatalf("RateLimitRPS=%d want=%d", cfg.RateLimitRPS, 75)
		}
		if cfg.RateLimitBurst != 25 {
			t.Fatalf("RateLimitBurst=%d want=%d", cfg.RateLimitBurst, 25)
		}
	})
}

func TestLoad_RateLimitEnvInvalidValuesIgnored(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_RATE_LIMIT_RPS", "not-a-number")
		t.Setenv("DIR2MCP_RATE_LIMIT_BURST", "-1")

		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if cfg.RateLimitRPS != 60 {
			t.Fatalf("RateLimitRPS=%d want default %d", cfg.RateLimitRPS, 60)
		}
		if cfg.RateLimitBurst != 20 {
			t.Fatalf("RateLimitBurst=%d want default %d", cfg.RateLimitBurst, 20)
		}
	})
}

func TestLoad_RateLimitEnvAllowsZeroToDisable(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_RATE_LIMIT_RPS", "0")
		t.Setenv("DIR2MCP_RATE_LIMIT_BURST", "0")

		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if cfg.RateLimitRPS != 0 {
			t.Fatalf("RateLimitRPS=%d want=%d", cfg.RateLimitRPS, 0)
		}
		if cfg.RateLimitBurst != 0 {
			t.Fatalf("RateLimitBurst=%d want=%d", cfg.RateLimitBurst, 0)
		}
	})
}

func TestDefault_TrustedProxies(t *testing.T) {
	cfg := config.Default()
	assertContains(t, cfg.TrustedProxies, "127.0.0.1/32")
	assertContains(t, cfg.TrustedProxies, "::1/128")
}

func TestLoad_TrustedProxiesEnvAppendsAndNormalizes(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_TRUSTED_PROXIES", "10.0.0.0/8,203.0.113.7")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertContains(t, cfg.TrustedProxies, "127.0.0.1/32")
		assertContains(t, cfg.TrustedProxies, "::1/128")
		assertContains(t, cfg.TrustedProxies, "10.0.0.0/8")
		assertContains(t, cfg.TrustedProxies, "203.0.113.7/32")
	})
}

func TestLoad_TrustedProxiesEnvSkipsMalformedValues(t *testing.T) {
	tmp := t.TempDir()

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_TRUSTED_PROXIES", "bad-value,10.0.0.0/8,300.1.1.1")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		assertContains(t, cfg.TrustedProxies, "10.0.0.0/8")
		assertNotContains(t, cfg.TrustedProxies, "bad-value")
		assertNotContains(t, cfg.TrustedProxies, "300.1.1.1")
	})
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, values)
}

func assertNotContains(t *testing.T, values []string, needle string) {
	t.Helper()
	for _, value := range values {
		if value == needle {
			t.Fatalf("did not expect %q in %v", needle, values)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
}
