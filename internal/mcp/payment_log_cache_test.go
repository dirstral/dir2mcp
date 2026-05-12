package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// badWriter implements io.Writer but always returns an error. It is used to
// trigger a flush failure inside Server.Close during testing. We expose the
// underlying error as a sentinel so tests can use errors.Is to verify that the
// joined error contains it.

var errWriteFailure = errors.New("write failure")

type badWriter struct{}

func (badWriter) Write(p []byte) (int, error) { return 0, errWriteFailure }

func TestAppendPaymentLogCachingAndClose(t *testing.T) {
	// create a temp state dir to hold the log
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.StateDir = tmp

	// new server starts with empty paymentLogPath because x402 is disabled;
	// manually initialize it for our test so appendPaymentLog actually does work.
	s := NewServer(cfg, nil)
	s.paymentLogPath = filepath.Join(tmp, "payments", "settlement.log")
	// make sure the directory doesn't yet exist
	if _, err := os.Stat(filepath.Join(tmp, "payments")); !os.IsNotExist(err) {
		t.Fatalf("expected payments directory to be absent")
	}

	// manually append a couple of entries; path should be created and writer cached
	s.appendPaymentLog("evt1", map[string]interface{}{"foo": "bar"})
	s.appendPaymentLog("evt2", map[string]interface{}{"baz": 123})

	// close to flush
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned unexpected error: %v", err)
	}

	// read file and verify two JSON lines
	logPath := filepath.Join(tmp, "payments", "settlement.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrong number of log lines: got %d, want 2", len(lines))
	}
	// build expectations matching the two appended entries.
	expectedEvents := []struct {
		name    string
		payload map[string]interface{}
	}{
		{"evt1", map[string]interface{}{"foo": "bar"}},
		{"evt2", map[string]interface{}{"baz": float64(123)}},
	}

	for i, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is not valid json: %v", i+1, err)
		}

		// verify event name
		evt, ok := entry["event"].(string)
		if !ok {
			t.Fatalf("line %d event field is not a string", i+1)
		}
		if evt != expectedEvents[i].name {
			t.Fatalf("line %d has event %q, want %q", i+1, evt, expectedEvents[i].name)
		}

		// payload is stored under "data" in the log entry
		payload, ok := entry["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("line %d data field has unexpected type", i+1)
		}
		if !reflect.DeepEqual(payload, expectedEvents[i].payload) {
			t.Fatalf("line %d payload mismatch: got %#v, want %#v", i+1, payload, expectedEvents[i].payload)
		}
	}
}

func TestAppendPaymentLogRecovery(t *testing.T) {
	// simulate a writer that fails on first write but can be reopened successfully
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.AuthMode = "none"

	s := NewServer(cfg, nil)
	// ensure server resources are cleaned up; mirrors production behavior
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close returned unexpected error: %v", err)
		}
	}()

	s.paymentLogPath = filepath.Join(tmp, "payments", "settlement.log")

	// create directory and an initial file so we can assign a real *os.File
	if err := os.MkdirAll(filepath.Dir(s.paymentLogPath), 0o755); err != nil {
		t.Fatalf("setup mkdirall failed: %v", err)
	}
	f, err := os.OpenFile(s.paymentLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("setup open file failed: %v", err)
	}
	// assign a bad writer backed by a valid file; the write should fail and
	// trigger recovery logic that reopens the path again with a good writer.
	s.paymentLogFile = f
	s.paymentLogWriter = bufio.NewWriter(badWriter{})

	var events []map[string]interface{}
	s.eventEmitter = func(level, event string, data interface{}) {
		events = append(events, map[string]interface{}{"level": level, "event": event, "data": data})
	}

	// perform append; recovery should occur internally
	s.appendPaymentLog("evt", map[string]interface{}{"a": 1})

	// after recovery the writer and file should be non-nil
	if s.paymentLogWriter == nil || s.paymentLogFile == nil {
		t.Fatalf("expected writer/file to be reinitialized after recovery")
	}

	// at least one warning should have been emitted about the initial write
	if len(events) == 0 {
		t.Fatalf("expected a warning event during recovery")
	}

	// read the file and ensure the entry was written
	data, err := os.ReadFile(s.paymentLogPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("wrong number of log lines: got %d, want 1", len(lines))
	}
}

func TestAppendPaymentLogMkdirAllFailure(t *testing.T) {
	// MkdirAll should fail when a regular file exists at the target path; no
	// fallback write should occur and no file should be created.
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.AuthMode = "none"

	s := NewServer(cfg, nil)
	// ensure server resources are cleaned up during the test
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close returned unexpected error: %v", err)
		}
	}()
	s.paymentLogPath = filepath.Join(tmp, "payments", "settlement.log")

	// create a file at the directory location so MkdirAll errors
	if err := os.WriteFile(filepath.Join(tmp, "payments"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup failure: %v", err)
	}

	var events []map[string]interface{}
	s.eventEmitter = func(level, event string, data interface{}) {
		events = append(events, map[string]interface{}{"level": level, "event": event, "data": data})
	}

	s.appendPaymentLog("evt", nil)

	// directory should still be a regular file
	fi, err := os.Stat(filepath.Join(tmp, "payments"))
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.IsDir() {
		t.Fatalf("expected payments to remain a file, not a directory")
	}

	// the log file should not exist because we never opened it.  stat can
	// fail because the parent component is a regular file rather than a
	// directory; treat any error other than "file exists" as success.  We
	// don't rely on a platform-specific message such as "not a directory".
	if fi, err := os.Stat(s.paymentLogPath); err == nil {
		t.Logf("unexpected log path exists: %v (isdir=%v)", fi.Name(), fi.IsDir())
		t.Fatalf("expected log file to not exist")
	}

	// Regardless of the stat result above (IsNotExist or other), we expect a
	// warning event to have been emitted when s.appendPaymentLog failed to
	// create the directory.  Log them for debugging if present.
	if len(events) == 0 {
		t.Fatalf("expected at least one warning event when MkdirAll fails")
	}
	for i, e := range events {
		t.Logf("event %d: %+v", i, e)
	}
}

func TestCloseErrorsPropagated(t *testing.T) {
	// create a server and manually assign a writer that will return an error
	// when flushed and a file that is already closed so Close() on it fails.
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.AuthMode = "none"

	s := NewServer(cfg, nil)

	// failing writer: underlying writer always returns an error.  Write a
	// byte so that Flush() will attempt to write it and trigger the error.
	s.paymentLogWriter = bufio.NewWriter(badWriter{})
	_, _ = s.paymentLogWriter.Write([]byte("x"))

	// prepare a file and close it immediately to force a close error later
	f, err := os.Create(filepath.Join(tmp, "dummy"))
	if err != nil {
		t.Fatalf("failed to create dummy file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close dummy file: %v", err)
	}
	// f is now closed; calling Close again yields an error
	s.paymentLogFile = f

	err = s.Close()
	if err == nil {
		t.Fatalf("expected error from Close when flush and file close fail")
	}
	// Ensure both underlying errors are present via errors.Is.
	if !errors.Is(err, errWriteFailure) {
		t.Fatalf("joined error did not contain flush error: %v", err)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("joined error did not contain file close error: %v", err)
	}
	// Verify that the returned error implements Unwrap() []error so we know it's
	// actually a joined error and that there are multiple components.  Prior to
	// adding a Sync() call we only expected two errors (flush+close), but the
	// sync step may also fail resulting in three entries.  Accept any count >=2.
	if je, ok := err.(interface{ Unwrap() []error }); ok {
		if len(je.Unwrap()) < 2 {
			t.Fatalf("expected at least two errors from joined error, got %d", len(je.Unwrap()))
		}
	} else {
		t.Fatalf("expected joined error type, got %T", err)
	}
	if s.paymentLogWriter != nil || s.paymentLogFile != nil {
		t.Fatalf("expected writer and file fields to be cleared after Close")
	}
}
