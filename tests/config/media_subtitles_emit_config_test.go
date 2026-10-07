package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaSubtitlesEmit_DefaultsOff pins SPEC §8.6.14: subtitle write-back is
// OFF by default and carries its spec defaults (formats [vtt], policy
// if_missing, no language restriction, beside-the-media output) so an
// enabled-but-unspecified config behaves predictably and an untouched config is
// unchanged.
func TestMediaSubtitlesEmit_DefaultsOff(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaSubtitlesEmitEnabled {
		t.Fatalf("media.subtitles.emit.enabled must default to false")
	}
	if !reflect.DeepEqual(cfg.MediaSubtitlesEmitFormats, []string{"vtt"}) {
		t.Fatalf("formats default = %v, want [vtt]", cfg.MediaSubtitlesEmitFormats)
	}
	if config.DefaultMediaSubtitlesEmitPolicy != "if_missing" {
		t.Fatalf("DefaultMediaSubtitlesEmitPolicy = %q, want if_missing", config.DefaultMediaSubtitlesEmitPolicy)
	}
	if cfg.MediaSubtitlesEmitPolicy != "if_missing" {
		t.Fatalf("policy default = %q, want if_missing", cfg.MediaSubtitlesEmitPolicy)
	}
	if len(cfg.MediaSubtitlesEmitLanguages) != 0 || cfg.MediaSubtitlesEmitDir != "" {
		t.Fatalf("languages/dir must default empty, got %v / %q", cfg.MediaSubtitlesEmitLanguages, cfg.MediaSubtitlesEmitDir)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default: %v", err)
	}
}

// TestMediaSubtitlesEmit_NormalizesFormatsPolicyLanguages pins the normalization
// the loader applies whether or not write-back is enabled: formats and languages
// are lower-cased, trimmed and deduped (order preserved), an empty format list
// falls back to [vtt], and the policy enum is case-insensitive.
func TestMediaSubtitlesEmit_NormalizesFormatsPolicyLanguages(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSubtitlesEmitFormats = []string{" SRT ", "vtt", "srt", ""}
	cfg.MediaSubtitlesEmitLanguages = []string{"RU", " en", "ru"}
	cfg.MediaSubtitlesEmitPolicy = " Refresh "
	cfg.MediaSubtitlesEmitDir = "  /out  "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := []string{"srt", "vtt"}; !reflect.DeepEqual(cfg.MediaSubtitlesEmitFormats, want) {
		t.Fatalf("formats = %v, want %v", cfg.MediaSubtitlesEmitFormats, want)
	}
	if want := []string{"ru", "en"}; !reflect.DeepEqual(cfg.MediaSubtitlesEmitLanguages, want) {
		t.Fatalf("languages = %v, want %v", cfg.MediaSubtitlesEmitLanguages, want)
	}
	if cfg.MediaSubtitlesEmitPolicy != "refresh" || cfg.MediaSubtitlesEmitDir != "/out" {
		t.Fatalf("policy/dir = %q/%q, want refresh//out", cfg.MediaSubtitlesEmitPolicy, cfg.MediaSubtitlesEmitDir)
	}

	empty := config.Default()
	empty.MediaSubtitlesEmitFormats = []string{}
	if err := empty.Validate(); err != nil {
		t.Fatalf("Validate empty formats: %v", err)
	}
	if !reflect.DeepEqual(empty.MediaSubtitlesEmitFormats, []string{"vtt"}) {
		t.Fatalf("empty formats must fall back to [vtt], got %v", empty.MediaSubtitlesEmitFormats)
	}
}

// TestMediaSubtitlesEmit_RejectsBadValues pins the CONFIG_INVALID gates: an
// unknown format, an unknown policy, ttml without the TTML surface (only when
// enabled), and an s3 source with no output dir (only when enabled). The last
// two must NOT fire while write-back is disabled: an unused surface never makes
// a configuration invalid.
func TestMediaSubtitlesEmit_RejectsBadValues(t *testing.T) {
	badFormat := config.Default()
	badFormat.MediaSubtitlesEmitFormats = []string{"vtt", "ass"}
	if err := badFormat.Validate(); err == nil || !strings.Contains(err.Error(), "media.subtitles.emit.formats") {
		t.Fatalf("unknown format must be rejected, got %v", err)
	}

	badPolicy := config.Default()
	badPolicy.MediaSubtitlesEmitPolicy = "always"
	if err := badPolicy.Validate(); err == nil || !strings.Contains(err.Error(), "media.subtitles.emit.policy") {
		t.Fatalf("unknown policy must be rejected, got %v", err)
	}

	ttmlOff := config.Default()
	ttmlOff.MediaSubtitlesEmitFormats = []string{"vtt", "ttml"}
	if err := ttmlOff.Validate(); err != nil {
		t.Fatalf("ttml in formats while write-back is DISABLED must be accepted, got %v", err)
	}
	ttmlOff.MediaSubtitlesEmitEnabled = true
	if err := ttmlOff.Validate(); err == nil || !strings.Contains(err.Error(), "media.subtitles.ttml.enabled") {
		t.Fatalf("ttml in formats without ttml.enabled must be rejected when enabled, got %v", err)
	}
	ttmlOff.MediaSubtitlesTTMLEnabled = true
	if err := ttmlOff.Validate(); err != nil {
		t.Fatalf("ttml with ttml.enabled must validate, got %v", err)
	}

	s3 := config.Default()
	s3.Source.Kind = "s3"
	s3.Source.S3Bucket = "corpus"
	s3.MediaSubtitlesEmitEnabled = true
	err := s3.Validate()
	if err == nil || !strings.Contains(err.Error(), "media.subtitles.emit.dir") {
		t.Fatalf("s3 source with write-back and no dir must be rejected, got %v", err)
	}
}

// TestMediaSubtitlesEmit_LoadsNestedAndFlatKeys pins that the nested
// `media.subtitles.emit.*` block (the SPEC §16.2 shape) and the flat
// `media_subtitles_emit_*` spellings both load, including the list keys in
// block form and in inline form.
func TestMediaSubtitlesEmit_LoadsNestedAndFlatKeys(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested.yaml")
	if err := os.WriteFile(nested, []byte(`
media:
  subtitles:
    ttml:
      enabled: true
    emit:
      enabled: true
      formats: [vtt, ttml]
      languages:
        - ru
        - en
      policy: refresh
      dir: /tmp/out
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(nested)
	if err != nil {
		t.Fatalf("LoadFile nested: %v", err)
	}
	if !cfg.MediaSubtitlesEmitEnabled || cfg.MediaSubtitlesEmitPolicy != "refresh" || cfg.MediaSubtitlesEmitDir != "/tmp/out" {
		t.Fatalf("nested scalars not loaded: enabled=%v policy=%q dir=%q", cfg.MediaSubtitlesEmitEnabled, cfg.MediaSubtitlesEmitPolicy, cfg.MediaSubtitlesEmitDir)
	}
	if want := []string{"vtt", "ttml"}; !reflect.DeepEqual(cfg.MediaSubtitlesEmitFormats, want) {
		t.Fatalf("nested formats = %v, want %v", cfg.MediaSubtitlesEmitFormats, want)
	}
	if want := []string{"ru", "en"}; !reflect.DeepEqual(cfg.MediaSubtitlesEmitLanguages, want) {
		t.Fatalf("nested languages = %v, want %v", cfg.MediaSubtitlesEmitLanguages, want)
	}

	flat := filepath.Join(dir, "flat.yaml")
	if err := os.WriteFile(flat, []byte(`
media_subtitles_emit_enabled: true
media_subtitles_emit_formats:
  - srt
media_subtitles_emit_policy: if_missing
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.LoadFile(flat)
	if err != nil {
		t.Fatalf("LoadFile flat: %v", err)
	}
	if !cfg.MediaSubtitlesEmitEnabled || !reflect.DeepEqual(cfg.MediaSubtitlesEmitFormats, []string{"srt"}) || cfg.MediaSubtitlesEmitPolicy != "if_missing" {
		t.Fatalf("flat keys not loaded: enabled=%v formats=%v policy=%q", cfg.MediaSubtitlesEmitEnabled, cfg.MediaSubtitlesEmitFormats, cfg.MediaSubtitlesEmitPolicy)
	}
}

// TestMediaSubtitlesEmit_DirMustBeOutsideCorpus pins SPEC §8.6.14: a non-empty
// output root inside the corpus (or equal to it) is CONFIG_INVALID when
// write-back is enabled, because it would place subtitles beside other media;
// a sibling or unrelated directory is accepted.
func TestMediaSubtitlesEmit_DirMustBeOutsideCorpus(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{root, filepath.Join(root, "subs"), filepath.Join(root, "a", "..", "b")} {
		cfg := config.Default()
		cfg.RootDir = root
		cfg.MediaSubtitlesEmitEnabled = true
		cfg.MediaSubtitlesEmitDir = bad
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside the corpus root") {
			t.Fatalf("dir %q inside corpus must be rejected, got %v", bad, err)
		}
	}
	for _, ok := range []string{t.TempDir(), root + "-subs"} {
		cfg := config.Default()
		cfg.RootDir = root
		cfg.MediaSubtitlesEmitEnabled = true
		cfg.MediaSubtitlesEmitDir = ok
		if err := cfg.Validate(); err != nil {
			t.Fatalf("dir %q outside corpus must validate, got %v", ok, err)
		}
	}
}

// TestMediaSubtitlesEmit_DirThroughSymlinkIntoCorpusIsRejected pins that the
// containment check follows symlinks: an output root that lies outside the
// corpus as written but reaches it through a symlink is inside, also when the
// final directory does not exist yet.
func TestMediaSubtitlesEmit_DirThroughSymlinkIntoCorpusIsRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "subs-link")
	if err := os.Symlink(filepath.Join(root, "media"), link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	for _, bad := range []string{link, filepath.Join(link, "not-yet", "created")} {
		cfg := config.Default()
		cfg.RootDir = root
		cfg.MediaSubtitlesEmitEnabled = true
		cfg.MediaSubtitlesEmitDir = bad
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside the corpus root") {
			t.Fatalf("dir %q reaches the corpus through a symlink and must be rejected, got %v", bad, err)
		}
	}
}
