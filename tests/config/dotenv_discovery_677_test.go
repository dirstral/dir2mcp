package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/secrets"
)

// #677: `config init` writes credentials to `.env.local` beside the resolved
// config file, but the loader read the working directory only. With a custom
// --config path the two directories differ, so a key the wizard reported as
// saved was invisible to the very next command. These tests pin the rule that
// closes the gap: the config file's directory is searched first, the working
// directory second.

// dotenvFixture builds the two-directory case: a config file in its own
// directory, and a different working directory. It also removes the two higher
// precedence sources (environment, keychain) so a test observes the dotenv
// layer alone.
func dotenvFixture(t *testing.T) (configDir, workDir, configPath string) {
	t.Helper()
	t.Setenv(secrets.DisableEnvVar, "1")
	t.Setenv("MISTRAL_API_KEY", "")

	configDir = t.TempDir()
	workDir = t.TempDir()
	chdir(t, workDir)

	configPath = filepath.Join(configDir, "custom.yaml")
	cfg := config.Default()
	cfg.RootDir = configDir
	cfg.StateDir = filepath.Join(configDir, ".dir2mcp")
	if err := config.SaveFile(configPath, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	return configDir, workDir, configPath
}

// loadWithConfig loads through the full precedence chain and returns the config
// plus the credential the provider resolver expands for the built-in mistral
// profile.
func loadWithConfig(t *testing.T, configPath string) (config.Config, string) {
	t.Helper()
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", configPath, err)
	}
	prof, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	return cfg, prof.APIKey
}

// The reported bug: a key saved beside a custom config file must be found from
// any working directory. Without the fix the loader reads the working
// directory only and this resolves to "".
func TestLoad_DotEnvLocalBesideCustomConfigIsLoaded(t *testing.T) {
	configDir, _, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=beside_config\n")

	if _, got := loadWithConfig(t, configPath); got != "beside_config" {
		t.Fatalf("key beside the config file was not loaded: got %q, want beside_config", got)
	}
}

// The plain `.env` beside a custom config file is read too, not just
// `.env.local`.
func TestLoad_DotEnvBesideCustomConfigIsLoaded(t *testing.T) {
	configDir, _, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env"), "MISTRAL_API_KEY=plain_beside_config\n")

	if _, got := loadWithConfig(t, configPath); got != "plain_beside_config" {
		t.Fatalf("plain .env beside the config file was not loaded: got %q, want plain_beside_config", got)
	}
}

// Regression guard: the working directory stays a search location, so an
// existing deployment that keeps `.env.local` next to the shell keeps working.
func TestLoad_DotEnvLocalInWorkingDirStillLoads(t *testing.T) {
	_, workDir, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(workDir, ".env.local"), "MISTRAL_API_KEY=working_dir\n")

	if _, got := loadWithConfig(t, configPath); got != "working_dir" {
		t.Fatalf("working-directory key was not loaded: got %q, want working_dir", got)
	}
}

// Pinned precedence, part 1: the config file's directory outranks the working
// directory for the same variable.
func TestLoad_ConfigDirWinsOverWorkingDir(t *testing.T) {
	configDir, workDir, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=beside_config\n")
	writeFile(t, filepath.Join(workDir, ".env.local"), "MISTRAL_API_KEY=working_dir\n")

	if _, got := loadWithConfig(t, configPath); got != "beside_config" {
		t.Fatalf("config directory must win: got %q, want beside_config", got)
	}
}

// Pinned precedence, part 2: inside one directory `.env.local` still outranks
// `.env`, and the whole config directory still outranks the working directory.
func TestLoad_DotEnvPrecedenceAcrossBothDirectories(t *testing.T) {
	configDir, workDir, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env"), "MISTRAL_API_KEY=config_dir_env\n")
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=config_dir_env_local\n")
	writeFile(t, filepath.Join(workDir, ".env"), "MISTRAL_API_KEY=work_dir_env\n")
	writeFile(t, filepath.Join(workDir, ".env.local"), "MISTRAL_API_KEY=work_dir_env_local\n")

	if _, got := loadWithConfig(t, configPath); got != "config_dir_env_local" {
		t.Fatalf("precedence order broken: got %q, want config_dir_env_local", got)
	}
}

// A variable that only the lower-precedence directory defines is still used:
// the two directories combine, they do not replace each other.
func TestLoad_WorkingDirFillsWhatConfigDirDoesNotDefine(t *testing.T) {
	configDir, workDir, configPath := dotenvFixture(t)
	t.Setenv("ELEVENLABS_API_KEY", "")
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=beside_config\n")
	writeFile(t, filepath.Join(workDir, ".env.local"), "ELEVENLABS_API_KEY=work_dir_only\n")

	cfg, got := loadWithConfig(t, configPath)
	if got != "beside_config" {
		t.Fatalf("config directory value lost: got %q, want beside_config", got)
	}
	if cfg.ElevenLabsAPIKey != "work_dir_only" {
		t.Fatalf("working-directory value lost: got %q, want work_dir_only", cfg.ElevenLabsAPIKey)
	}
}

// An environment variable still beats every dotenv file (SPEC §16.1.1 #1).
func TestLoad_EnvBeatsDotEnvBesideConfig(t *testing.T) {
	configDir, _, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=beside_config\n")
	t.Setenv("MISTRAL_API_KEY", "from_env")

	if _, got := loadWithConfig(t, configPath); got != "from_env" {
		t.Fatalf("environment must win: got %q, want from_env", got)
	}
}

// No dotenv file anywhere is a normal setup, not an error.
func TestLoad_MissingDotEnvFilesIsNotAnError(t *testing.T) {
	_, _, configPath := dotenvFixture(t)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("absent dotenv files must not fail the load: %v", err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "dotenv") {
			t.Fatalf("unexpected dotenv warning with no dotenv file present: %v", w)
		}
	}
}

// Two directories in play is the situation the operator cannot otherwise see,
// so the loader says it. The warning names the files, in precedence order, and
// carries no variable name and no value.
func TestLoad_WarnsWhenDotEnvFilesLiveInTwoDirectories(t *testing.T) {
	configDir, workDir, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=beside_config\n")
	writeFile(t, filepath.Join(workDir, ".env.local"), "MISTRAL_API_KEY=working_dir\n")

	cfg, _ := loadWithConfig(t, configPath)

	var warning string
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "dotenv files loaded from") {
			warning = w.Error()
			break
		}
	}
	if warning == "" {
		t.Fatalf("expected a dotenv multi-directory warning, got %v", cfg.Warnings)
	}
	configEntry := filepath.Join(configDir, ".env.local")
	at := strings.Index(warning, configEntry)
	if at < 0 {
		t.Fatalf("warning must name the config-directory file %q: %s", configEntry, warning)
	}
	// The working-directory entry must follow it: the list is the precedence.
	if !strings.Contains(warning[at+len(configEntry):], ".env.local") {
		t.Fatalf("warning must list the working-directory file after the config one: %s", warning)
	}
	// The whole point of the file is to hold secrets. The report is paths only.
	for _, forbidden := range []string{"beside_config", "working_dir", "MISTRAL_API_KEY"} {
		if strings.Contains(warning, forbidden) {
			t.Fatalf("warning leaked %q: %s", forbidden, warning)
		}
	}
}

// One directory holding both files is the ordinary setup and stays quiet.
func TestLoad_NoMultiDirWarningForASingleDirectory(t *testing.T) {
	configDir, _, configPath := dotenvFixture(t)
	writeFile(t, filepath.Join(configDir, ".env"), "MISTRAL_API_KEY=config_dir_env\n")
	writeFile(t, filepath.Join(configDir, ".env.local"), "MISTRAL_API_KEY=config_dir_env_local\n")

	cfg, _ := loadWithConfig(t, configPath)
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "dotenv files loaded from") {
			t.Fatalf("single-directory setup must not warn: %v", w)
		}
	}
}

// The default case (config file in the working directory) reads one directory,
// so it must not warn about two. Both spellings of the same path count: the
// relative default and an absolute --config that points at the working
// directory must collapse to one directory (a temp directory on macOS is
// reached through a symlink, so the absolute case also pins the symlink
// resolution).
func TestLoad_ConfigInWorkingDirDoesNotWarn(t *testing.T) {
	for _, tc := range []struct {
		name          string
		configPathFor func(dir string) string
	}{
		{"relative default", func(string) string { return ".dir2mcp.yaml" }},
		{"absolute config flag", func(dir string) string { return filepath.Join(dir, ".dir2mcp.yaml") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(secrets.DisableEnvVar, "1")
			t.Setenv("MISTRAL_API_KEY", "")
			dir := t.TempDir()
			chdir(t, dir)
			writeFile(t, filepath.Join(dir, ".env.local"), "MISTRAL_API_KEY=same_dir\n")

			cfg, err := config.Load(tc.configPathFor(dir))
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w.Error(), "dotenv files loaded from") {
					t.Fatalf("config in the working directory must not warn: %v", w)
				}
			}
			prof, err := cfg.Providers().Resolve(provider.CapEmbed)
			if err != nil {
				t.Fatalf("resolve embed: %v", err)
			}
			if prof.APIKey != "same_dir" {
				t.Fatalf("default-path key lost: got %q, want same_dir", prof.APIKey)
			}
		})
	}
}

// A dotenv file beside the config file that exists but cannot be read is
// reported, not skipped. Silence is the bug this issue is about.
func TestLoad_UnreadableDotEnvBesideConfigIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission semantics differ on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	configDir, _, configPath := dotenvFixture(t)
	bad := filepath.Join(configDir, ".env.local")
	writeFile(t, bad, "MISTRAL_API_KEY=beside_config\n")
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })

	_, err := config.Load(configPath)
	if err == nil {
		t.Fatal("an unreadable dotenv file must fail the load, not pass silently")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error must name the file: %v", err)
	}
	if strings.Contains(err.Error(), "beside_config") {
		t.Fatalf("error leaked the file contents: %v", err)
	}
}
