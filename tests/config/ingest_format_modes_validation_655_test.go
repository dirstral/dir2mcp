package tests

// Per-format ingest modes are validated closed enums: dir2mcp #655.
//
// `ingest.pdf.mode`, `ingest.images.mode`, `ingest.audio.mode` and
// `ingest.archives.mode` are advertised in the SPEC §16.2 config template as
// closed sets (off|ocr|auto, off|ocr_auto|ocr_on, off|auto|on, off|shallow|deep).
// Nothing validated them, so `ingest.archives.mode: shalow` loaded exactly as
// happily as `deep`, and the operator could believe a provider-cost or privacy
// setting was in force when no value at all had been understood.
//
// Validate() is the gate, so the check covers every entry point: a persisted
// config, the CLI flag overlay and the save paths.
//
// What each accepted mode DOES is still not implemented, and that is deliberate.
// The canonical spec names the enum members without defining the behavior of any
// member, so implementing one would author normative semantics this repository
// does not own. These tests therefore pin the validation contract only, and the
// README states plainly that the modes have no runtime effect yet.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// ingestModeCase describes one key: its YAML spelling, a setter, every advertised
// value, and a plausible typo.
type ingestModeCase struct {
	key      string
	yamlKey  string
	set      func(*config.Config, string)
	get      func(config.Config) string
	accepted []string
	typo     string
}

func ingestModeCases() []ingestModeCase {
	return []ingestModeCase{
		{
			key: "ingest.pdf.mode", yamlKey: "ingest_pdf_mode",
			set: func(c *config.Config, v string) { c.IngestPDFMode = v },
			get: func(c config.Config) string { return c.IngestPDFMode },
			// `auto` is advertised; the typo is a value that looks reasonable.
			accepted: []string{"off", "ocr", "auto"}, typo: "ocr_auto",
		},
		{
			key: "ingest.images.mode", yamlKey: "ingest_images_mode",
			set:      func(c *config.Config, v string) { c.IngestImagesMode = v },
			get:      func(c config.Config) string { return c.IngestImagesMode },
			accepted: []string{"off", "ocr_auto", "ocr_on"}, typo: "ocr",
		},
		{
			key: "ingest.audio.mode", yamlKey: "ingest_audio_mode",
			set:      func(c *config.Config, v string) { c.IngestAudioMode = v },
			get:      func(c config.Config) string { return c.IngestAudioMode },
			accepted: []string{"off", "auto", "on"}, typo: "yes",
		},
		{
			key: "ingest.archives.mode", yamlKey: "ingest_archives_mode",
			set:      func(c *config.Config, v string) { c.IngestArchivesMode = v },
			get:      func(c config.Config) string { return c.IngestArchivesMode },
			accepted: []string{"off", "shallow", "deep"}, typo: "shalow",
		},
	}
}

// TestIngestFormatModes_TypoIsRejected is the headline case. Each key gets a
// plausible wrong value, including one borrowed from a NEIGHBOURING key's enum,
// which is the mistake an operator is most likely to make.
func TestIngestFormatModes_TypoIsRejected(t *testing.T) {
	for _, tc := range ingestModeCases() {
		cfg := config.Default()
		tc.set(&cfg, tc.typo)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s=%q must be rejected; Validate returned no error", tc.key, tc.typo)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("the error for %s must name the key so an operator can fix it; got %v", tc.key, err)
		}
		for _, accepted := range tc.accepted {
			if !strings.Contains(err.Error(), accepted) {
				t.Errorf("the error for %s must list the accepted value %q; got %v", tc.key, accepted, err)
			}
		}
	}
}

// TestIngestFormatModes_AdvertisedValuesAreAccepted is the false-positive guard:
// every value the canonical template advertises must validate. A gate that
// refused a documented value would be worse than the missing check.
func TestIngestFormatModes_AdvertisedValuesAreAccepted(t *testing.T) {
	for _, tc := range ingestModeCases() {
		for _, value := range tc.accepted {
			cfg := config.Default()
			tc.set(&cfg, value)
			if err := cfg.Validate(); err != nil {
				t.Errorf("%s=%q is advertised by the spec template and must validate: %v", tc.key, value, err)
			}
		}
	}
}

// TestIngestFormatModes_EmptyKeepsTheDefault pins that an absent value still
// resolves to the documented default, so a bare config validates unchanged.
func TestIngestFormatModes_EmptyKeepsTheDefault(t *testing.T) {
	defaults := config.Default()
	for _, tc := range ingestModeCases() {
		cfg := config.Default()
		tc.set(&cfg, "")
		if err := cfg.Validate(); err != nil {
			t.Errorf("an empty %s must validate: %v", tc.key, err)
			continue
		}
		if got, want := tc.get(cfg), tc.get(defaults); got != want {
			t.Errorf("an empty %s must resolve to the default %q; got %q", tc.key, want, got)
		}
	}
}

// TestIngestFormatModes_ValueIsNormalized pins the canonical stored form, so a
// consumer of the effective config compares against one spelling.
func TestIngestFormatModes_ValueIsNormalized(t *testing.T) {
	for _, tc := range ingestModeCases() {
		cfg := config.Default()
		tc.set(&cfg, "  "+strings.ToUpper(tc.accepted[0])+" ")
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s must accept a padded upper-case value: %v", tc.key, err)
			continue
		}
		if got := tc.get(cfg); got != tc.accepted[0] {
			t.Errorf("%s must normalize to %q; got %q", tc.key, tc.accepted[0], got)
		}
	}
}

// TestIngestFormatModes_LoadFailsOnATypo is the startup-error proof: the gate
// runs on the load path, so a persisted typo stops the server instead of being
// accepted in silence.
func TestIngestFormatModes_LoadFailsOnATypo(t *testing.T) {
	for _, tc := range ingestModeCases() {
		dir := t.TempDir()
		path := filepath.Join(dir, ".dir2mcp.yaml")
		body := "root_dir: .\n" + tc.yamlKey + ": " + tc.typo + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("loading a config with %s: %s must fail", tc.yamlKey, tc.typo)
		} else if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("the load error must name %s; got %v", tc.key, err)
		}
	}
}

// TestIngestFormatModes_LoadFailsOnANestedTypo covers the nested spelling the
// canonical template and the README both use, which reaches the same gate through
// the `ingest.archives` section.
func TestIngestFormatModes_LoadFailsOnANestedTypo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	body := "root_dir: .\ningest:\n  archives:\n    mode: shalow\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("loading a config with a nested ingest.archives.mode typo must fail")
	}
	if !strings.Contains(err.Error(), "ingest.archives.mode") {
		t.Fatalf("the load error must name ingest.archives.mode; got %v", err)
	}
}

// TestIngestFormatModes_SaveRefusesATypo keeps a bad value out of a persisted
// file, which is the other half of gating in Validate() rather than in a command.
func TestIngestFormatModes_SaveRefusesATypo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	cfg.IngestArchivesMode = "shalow"
	if err := config.SaveFile(path, cfg); err == nil {
		t.Fatal("SaveFile must refuse an unrecognized ingest.archives.mode")
	}
}
