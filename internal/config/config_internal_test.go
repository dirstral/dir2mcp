package config

import (
	"strings"
	"testing"
)

func TestParseSecretSourceMetadata_SyntaxError(t *testing.T) {
	_, err := parseSecretSourceMetadata([]byte("secret_sources:\n  mistral_api_key env\n"))
	if err == nil {
		t.Fatal("expected syntax error")
	}
	if !strings.Contains(err.Error(), "expected key:value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseSecretSourceMetadata_ScannerError(t *testing.T) {
	// bufio.Scanner uses a fixed token limit by default; this verifies we
	// propagate scanner.Err() instead of silently swallowing it.
	longValue := strings.Repeat("a", 70*1024)
	raw := []byte("secret_sources:\n  mistral_api_key: " + longValue + "\n")
	_, err := parseSecretSourceMetadata(raw)
	if err == nil {
		t.Fatal("expected scanner error")
	}
	if !strings.Contains(err.Error(), "scan snapshot metadata") {
		t.Fatalf("unexpected scanner error: %v", err)
	}
}
