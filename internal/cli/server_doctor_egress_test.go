package cli

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestHostIsLocal_Classification proves the egress check's host classifier
// treats loopback, private/LAN, single-label, and known-internal hosts as
// non-egress while public FQDNs and public IPs are egress.
func TestHostIsLocal_Classification(t *testing.T) {
	local := []string{
		"localhost", "127.0.0.1", "127.5.6.7", "::1", "0.0.0.0",
		"10.0.0.5", "172.16.4.9", "192.168.1.20", "169.254.10.1",
		"gpu-vps", "gpu-vps.local", "models.internal", "box.lan",
	}
	for _, h := range local {
		if !hostIsLocal(h) {
			t.Errorf("hostIsLocal(%q) = false, want true (no egress)", h)
		}
	}
	public := []string{
		"api.mistral.ai", "api.openai.com", "generativelanguage.googleapis.com",
		"8.8.8.8", "example.com", "sub.domain.example.org",
	}
	for _, h := range public {
		if hostIsLocal(h) {
			t.Errorf("hostIsLocal(%q) = true, want false (egress)", h)
		}
	}
}

// TestHostFromBaseURL covers scheme-ful and scheme-less base_urls, port
// stripping, and case folding.
func TestHostFromBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.mistral.ai/v1":  "api.mistral.ai",
		"http://127.0.0.1:11434/v1":  "127.0.0.1",
		"http://GPU-VPS:9001":        "gpu-vps",
		"gpu-vps:9001":               "gpu-vps",
		"https://API.OpenAI.com/v1/": "api.openai.com",
	}
	for in, want := range cases {
		if got := hostFromBaseURL(in); got != want {
			t.Errorf("hostFromBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEffectiveProviderHost_KindDefaults proves a blank base_url resolves to
// the kind's cloud default (so a happy-path cloud profile still counts as
// egress) while a self-hosted-only kind with no base_url resolves to "".
func TestEffectiveProviderHost_KindDefaults(t *testing.T) {
	if got := effectiveProviderHost(provider.Profile{Kind: provider.KindMistral}); got != "api.mistral.ai" {
		t.Errorf("mistral default host = %q, want api.mistral.ai", got)
	}
	if got := effectiveProviderHost(provider.Profile{Kind: provider.KindOpenAI, BaseURL: "http://gpu:8080/v1"}); got != "gpu" {
		t.Errorf("explicit base_url host = %q, want gpu", got)
	}
	if got := effectiveProviderHost(provider.Profile{Kind: provider.KindWhisper}); got != "" {
		t.Errorf("self-hosted kind with no base_url host = %q, want \"\"", got)
	}
}

// TestKindDefaultHost_MirrorsClients guards against a provider kind gaining a
// cloud default that the egress check forgets to classify. Every cloud kind in
// the map must be non-empty.
func TestKindDefaultHost_MirrorsClients(t *testing.T) {
	for k, h := range kindDefaultHost {
		if strings.TrimSpace(h) == "" {
			t.Errorf("kindDefaultHost[%q] is empty", k)
		}
		if hostIsLocal(h) {
			t.Errorf("kindDefaultHost[%q] = %q classified as local; a cloud default must be public", k, h)
		}
	}
}
