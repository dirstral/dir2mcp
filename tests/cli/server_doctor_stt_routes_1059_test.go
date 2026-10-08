package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (dir2mcp #1059): `dir2mcp doctor` reports the resolved STT
// route table as the stt_routes check.

const sttRoutesBase = `root_dir: .
state_dir: .dir2mcp
stt_provider: whisper
providers:
  whisper:
    kind: whisper
    base_url: "http://127.0.0.1:1"
    stt_model: base-model
  ckpt-fa:
    kind: whisper
    base_url: "http://127.0.0.1:1"
    stt_model: model-fa
    stt_validation:
      - {language: fa, method: wer, sample: 100 read sentences, score: "13.3", date: 2026-10-06}
  ckpt-kk:
    kind: whisper
    base_url: "http://127.0.0.1:1"
    stt_model: model-kk
  lid:
    kind: whisper
    base_url: "http://127.0.0.1:1"
    stt_model: lid-model
`

func sttRoutesCheck(t *testing.T, cfgBody string) (string, string) {
	t.Helper()
	isolateFromAmbientCredentials(t)
	dir := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := findCheck(runDoctorReport(t, dir), "stt_routes")
	if c == nil {
		t.Fatal("doctor produced no stt_routes check")
	}
	return c.Status, c.Detail
}

func TestServerDoctor_STTRoutes_NoRoutesIsReportedPositively(t *testing.T) {
	status, detail := sttRoutesCheck(t, "root_dir: .\nstate_dir: .dir2mcp\n")
	if status != "ok" || !strings.Contains(detail, "no language routes configured") {
		t.Fatalf("status=%s detail=%q, want ok and a positive no-routes statement", status, detail)
	}
}

func TestServerDoctor_STTRoutes_ListsIdentifierCandidatesModelsAndPolicy(t *testing.T) {
	status, detail := sttRoutesCheck(t, sttRoutesBase+`media:
  stt:
    language_identifier: lid
    language_probe_sec: 20
    on_route_error: default
    language_providers:
      fa: ckpt-fa
      kk: [ckpt-kk, ckpt-fa]
`)
	if status != "ok" {
		t.Fatalf("status=%s detail=%q, want ok", status, detail)
	}
	for _, want := range []string{
		"identifier lid (lid-model) under item scope, probe 20s",
		"fa -> ckpt-fa (model-fa) validated [wer, 100 read sentences, 13.3, 2026-10-06]",
		"kk -> ckpt-kk (model-kk) > ckpt-fa (model-fa)",
		"on_route_error=default",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
}

func TestServerDoctor_STTRoutes_WarnsWhenNoCandidateIsEligible(t *testing.T) {
	status, detail := sttRoutesCheck(t, sttRoutesBase+`media:
  stt:
    require_validation: true
    language_providers:
      fa: ckpt-fa
      kk: ckpt-kk
`)
	if status != "warn" {
		t.Fatalf("status=%s detail=%q, want warn: kk has no validated candidate", status, detail)
	}
	for _, want := range []string{"ckpt-kk (model-kk) INELIGIBLE: no stt_validation record for kk", "No eligible candidate for kk", "default profile whisper decodes them"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	if strings.Contains(detail, "No eligible candidate for fa") {
		t.Errorf("fa has a validated candidate and must not be named: %q", detail)
	}
}
