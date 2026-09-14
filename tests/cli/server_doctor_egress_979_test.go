package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `egress` doctor check makes an ABSOLUTE claim: "no third-party egress".
// A claim like that is only worth making if every content-carrying path was
// examined, and until #979 two were not. There was no test over this check at
// all, which is how the omission survived.

// egressDetail runs doctor in a temp corpus built from cfgBody and returns the
// egress check's status and detail.
func egressDetail(t *testing.T, cfgBody string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// runDoctorReport already enters dir through testutil.WithWorkingDir, whose
	// cwd mutex is NOT reentrant: wrapping this in a second one deadlocks.
	var status, detail string
	for _, c := range runDoctorReport(t, dir) {
		if c.Name == "egress" {
			status, detail = c.Status, c.Detail
		}
	}
	if status == "" {
		t.Fatalf("doctor produced no egress check")
	}
	return status, detail
}

// localStack is embed + chat + stt all pointed at loopback: the shape a
// data-residency operator believes they are running.
const localStack = `root_dir: .
state_dir: .dir2mcp
providers:
  local-embed:
    kind: openai
    base_url: "http://127.0.0.1:8080/v1"
    embed_text_model: local-model
  local-chat:
    kind: openai
    base_url: "http://127.0.0.1:8081/v1"
    chat_model: local-chat
  whisper:
    kind: whisper
    base_url: "http://127.0.0.1:9000"
model:
  embed:
    provider: local-embed
  chat:
    provider: local-chat
stt:
  provider: whisper
`

func TestEgress_APurelyLocalStackIsReportedCleanAndNamesWhatItChecked(t *testing.T) {
	// A claim is only as good as its scope, so the clean verdict states the
	// scope. Without that, a later capability can be added to the product and
	// silently left out of the claim, which is exactly how #979 happened.
	status, detail := egressDetail(t, localStack)
	if status != "ok" {
		t.Fatalf("status = %q, want ok: every provider is loopback. detail=%q", status, detail)
	}
	if !strings.Contains(detail, "no third-party egress") {
		t.Errorf("detail = %q, want the clean verdict", detail)
	}
	for _, capability := range []string{"embed", "chat", "ocr", "stt", "rerank", "tts"} {
		if !strings.Contains(detail, capability) {
			t.Errorf("the verdict does not say it checked %q: %q", capability, detail)
		}
	}
}

func TestEgress_ARerankerSendsChunkTextsAndMustBeNamed(t *testing.T) {
	// The #979 case. Everything local EXCEPT a cohere reranker, which is sent
	// the candidate chunk texts. Before the fix this reported "no third-party
	// egress" while every query shipped corpus content to api.cohere.com.
	//
	// The credential comes from the ENVIRONMENT, not a literal in the config,
	// and that is the scenario rather than a style preference: rerank activates
	// on credential presence, so the case that bites is a key exported for
	// another project with nothing in the config naming it. A hardcoded
	// `api_key` would exercise a different activation route than the one this
	// check exists to catch.
	t.Setenv("COHERE_API_KEY", "test-key")

	cfg := localStack + `rerank:
  enabled: true
  provider: cohere
`
	status, detail := egressDetail(t, cfg)
	if strings.Contains(detail, "no third-party egress") {
		t.Fatalf("a cohere reranker was certified as no egress: %q", detail)
	}
	if !strings.Contains(detail, "api.cohere.com") {
		t.Errorf("the destination is not named: %q", detail)
	}
	if !strings.Contains(detail, "rerank") {
		t.Errorf("the capability is not named: %q", detail)
	}
	_ = status
}

func TestEgress_AnUnreadableEndpointIsNotCountedAsLocal(t *testing.T) {
	// `effectiveProviderHost` returns "" both for a self-hosted kind with no
	// base_url and for a base_url this check could not parse. Folding the second
	// into the clean verdict is a guess, and the guess runs in the reassuring
	// direction: the operator NAMED a destination and we failed to read it.
	cfg := `root_dir: .
state_dir: .dir2mcp
providers:
  broken:
    kind: openai
    base_url: "http://[::1"
    embed_text_model: m
model:
  embed:
    provider: broken
`
	status, detail := egressDetail(t, cfg)
	if status == "ok" && strings.Contains(detail, "no third-party egress") {
		t.Fatalf("an unparseable endpoint was certified as local: %q", detail)
	}
	if !strings.Contains(detail, "cannot say where content goes") {
		t.Errorf("the check does not admit what it does not know: %q", detail)
	}
	if !strings.Contains(detail, "embed") {
		t.Errorf("the affected capability is not named: %q", detail)
	}
}

func TestEgress_AMistypedPublicEndpointIsNotTurnedIntoALanHost(t *testing.T) {
	// The nastiest form of the previous test, and the reason the guard is in
	// hostFromBaseURL rather than only in the caller.
	//
	// The scheme-less reparse used to run on ANY unparseable value. Given
	// "http://[::1" it reparsed "http://http://[::1", whose host is "http" — a
	// bare single-label name, which hostIsLocal treats as LAN. So a typo in a
	// PUBLIC endpoint did not merely go unexamined: it was actively converted
	// into evidence of locality and folded into the clean verdict.
	// A SINGLE slash is enough to lose the host: url.Parse("https:/x") succeeds
	// with scheme "https" and an empty host, so a "://" test misses it.
	for _, bad := range []string{"http://[::1", "https://exa mple.com", "https:/api.example.com", "http:/[::1"} {
		cfg := `root_dir: .
state_dir: .dir2mcp
providers:
  broken:
    kind: openai
    base_url: "` + bad + `"
    embed_text_model: m
model:
  embed:
    provider: broken
`
		_, detail := egressDetail(t, cfg)
		if strings.Contains(detail, "no third-party egress") {
			t.Errorf("%q was certified as no egress: %q", bad, detail)
		}
	}
}

func TestEgress_TheDocumentedSchemelessHostPortStillResolves(t *testing.T) {
	// The guard on the reparse has to key on a TRANSPORT scheme, not on any
	// scheme. url.Parse reads the documented scheme-less form "gpu-vps:9001" as
	// scheme "gpu-vps" with an empty host, so rejecting every scheme-bearing
	// value would break exactly the case the reparse exists for, and a LAN
	// endpoint would start reporting as unreadable.
	cfg := `root_dir: .
state_dir: .dir2mcp
providers:
  lan-embed:
    kind: openai
    base_url: "gpu-vps:9001"
    embed_text_model: m
model:
  embed:
    provider: lan-embed
`
	status, detail := egressDetail(t, cfg)
	if status != "ok" || !strings.Contains(detail, "no third-party egress") {
		t.Errorf("a scheme-less LAN host:port no longer resolves: status=%q detail=%q", status, detail)
	}
}

func TestEgress_AnUnreadableEndpointDoesNotHideAKnownPublicOne(t *testing.T) {
	// Uncertainty must not outrank a CONFIRMED destination. Returning only the
	// warning would trade a known fact for a caveat: the operator would lose
	// sight of the host we positively know receives corpus content.
	t.Setenv("OPENAI_API_KEY", "test-key")

	cfg := `root_dir: .
state_dir: .dir2mcp
providers:
  public-embed:
    kind: openai
    embed_text_model: m
  broken-chat:
    kind: openai
    base_url: "https:/api.example.com"
    chat_model: c
model:
  embed:
    provider: public-embed
  chat:
    provider: broken-chat
`
	_, detail := egressDetail(t, cfg)
	if !strings.Contains(detail, "api.openai.com") {
		t.Errorf("the confirmed public destination was hidden by the uncertainty: %q", detail)
	}
	if !strings.Contains(detail, "cannot say where content goes") {
		t.Errorf("the uncertainty is not reported: %q", detail)
	}
}

func TestEgress_ACredentialAloneEnablesTTSEgressAndMustBeNamed(t *testing.T) {
	// The clean-path test only proves the word "tts" appears in a string
	// literal. It would keep passing if the TTS capability scan were removed,
	// which is no coverage at all for the thing #979 was about.
	//
	// This drives the resolver, and through the path that actually bites: TTS
	// has no explicit binding, so the built-in elevenlabs profile activates on
	// CREDENTIAL PRESENCE alone. That is the same shape as the rerank case — a
	// key exported for another project, nothing in the config naming it, and
	// content leaving the machine.
	//
	// STT stays pinned to loopback whisper, so the host below is named for tts.
	t.Setenv("ELEVENLABS_API_KEY", "test-key")

	_, detail := egressDetail(t, localStack)
	if strings.Contains(detail, "no third-party egress") {
		t.Fatalf("a credential-activated cloud TTS provider was certified as no egress: %q", detail)
	}
	if !strings.Contains(detail, "api.elevenlabs.io") {
		t.Errorf("the TTS destination is not named: %q", detail)
	}
	if !strings.Contains(detail, "tts") {
		t.Errorf("the tts capability is not named against the host: %q", detail)
	}
}
