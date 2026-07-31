package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// decodeCLIErrorCode extracts the machine-readable `code` from a JSON CLI error
// payload written to stderr.
func decodeCLIErrorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode CLI error payload %q: %v", raw, err)
	}
	return payload.Error.Code
}

// TestBindServerListener_EmitsBindFailed proves a failed listener bind surfaces
// the canonical §14.1 BIND_FAILED code (not the coarser exit-code label) in the
// JSON error payload.
func TestBindServerListener_EmitsBindFailed(t *testing.T) {
	// Occupy a port so the subsequent bind on the same address fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	var stderr bytes.Buffer
	a := &App{stderr: &stderr}
	cfg := config.Default()
	cfg.ListenAddr = occupied.Addr().String()
	cfg.StateDir = t.TempDir() // no recorded preferred port → no fallback

	ln, code := a.bindServerListener(cfg, true)
	if ln != nil {
		_ = ln.Close()
		t.Fatal("expected bind to fail on an occupied port")
	}
	if code == exitSuccess {
		t.Fatalf("expected a non-success exit code, got %d", code)
	}
	if got := decodeCLIErrorCode(t, stderr.Bytes()); got != protocol.ErrorCodeBindFailed {
		t.Fatalf("error code = %q, want %q (body=%s)", got, protocol.ErrorCodeBindFailed, stderr.String())
	}
}

// TestApplyTLSConfig_EmitsTLSConfigInvalid proves a TLS flag validation failure
// surfaces the canonical §14.1 TLS_CONFIG_INVALID code, distinct from the
// generic CONFIG_INVALID.
func TestApplyTLSConfig_EmitsTLSConfigInvalid(t *testing.T) {
	var stderr bytes.Buffer
	a := &App{stderr: &stderr}
	cfg := config.Default()

	// Cert without key is an invalid TLS configuration.
	opts := upOptions{tlsCert: filepath.Join(t.TempDir(), "cert.pem")}
	opts.jsonOutput = true

	_, _, code := a.applyTLSConfig(&cfg, opts)
	if code == exitSuccess {
		t.Fatal("expected TLS validation to fail")
	}
	if got := decodeCLIErrorCode(t, stderr.Bytes()); got != protocol.ErrorCodeTLSConfigInvalid {
		t.Fatalf("error code = %q, want %q (body=%s)", got, protocol.ErrorCodeTLSConfigInvalid, stderr.String())
	}
}

// TestWriteStoreInitError_EmitsIndexVersionMismatch proves an index-format
// mismatch reaches machine consumers as the canonical §14.3
// INDEX_VERSION_MISMATCH code rather than the coarse exit-code label.
//
// This is the gap #422 V6 called "CLI prose only": the store already raised a
// typed *store.IndexVersionMismatchError whose Code() was correct, but every
// Init call site funnelled it through writeCLIError, which derives the JSON
// `code` from the exit code. The canonical code survived only as a prefix
// inside the human-readable message, so a machine consumer saw GENERIC_ERROR.
// The pre-existing test asserted Code() on the raw Go error and never crossed
// the CLI boundary, which is why the gap went undetected.
func TestWriteStoreInitError_EmitsIndexVersionMismatch(t *testing.T) {
	mismatch := &store.IndexVersionMismatchError{Persisted: "1", Expected: "2"}

	var stderr bytes.Buffer
	writeStoreInitError(&stderr, true, exitIndexLoadFailure, mismatch,
		fmt.Sprintf("initialize metadata store: %v", mismatch))

	if got := decodeCLIErrorCode(t, stderr.Bytes()); got != protocol.ErrorCodeIndexVersionMismatch {
		t.Errorf("code = %q, want %q\npayload: %s",
			got, protocol.ErrorCodeIndexVersionMismatch, stderr.String())
	}
}

// TestWriteStoreInitError_WrappedMismatchStillDetected covers the realistic
// path: a store implementation that wraps the sentinel before returning it.
// errors.As must still find it, or the fix only works for the one backend that
// happens to return the error bare.
func TestWriteStoreInitError_WrappedMismatchStillDetected(t *testing.T) {
	wrapped := fmt.Errorf("open corpus: %w",
		&store.IndexVersionMismatchError{Persisted: "1", Expected: "2"})

	var stderr bytes.Buffer
	writeStoreInitError(&stderr, true, exitIndexLoadFailure, wrapped, "initialize metadata store")

	if got := decodeCLIErrorCode(t, stderr.Bytes()); got != protocol.ErrorCodeIndexVersionMismatch {
		t.Errorf("wrapped mismatch: code = %q, want %q", got, protocol.ErrorCodeIndexVersionMismatch)
	}
}

// TestWriteStoreInitError_UnrelatedErrorKeepsGenericCode is the false-positive
// guard: an ordinary Init failure must NOT be relabelled as a version mismatch.
func TestWriteStoreInitError_UnrelatedErrorKeepsGenericCode(t *testing.T) {
	var stderr bytes.Buffer
	writeStoreInitError(&stderr, true, exitIndexLoadFailure,
		errors.New("disk full"), "initialize metadata store: disk full")

	if got := decodeCLIErrorCode(t, stderr.Bytes()); got == protocol.ErrorCodeIndexVersionMismatch {
		t.Errorf("unrelated error was mislabelled %q", got)
	}
}
