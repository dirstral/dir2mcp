package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestDown_NoPidFile_IsIdempotent: dir2mcp down on a directory with no
// pid file should report informationally and exit 0 so teardown scripts
// can run unconditionally.
func TestDown_NoPidFile_IsIdempotent(t *testing.T) {
	root, _ := newDownFixture(t)
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, root, func() {
		code := app.RunWithContext(context.Background(), []string{"down"})
		if code != 0 {
			t.Fatalf("exit code: got=%d stderr=%s", code, stderr.String())
		}
	})
	if got := stdout.String(); !strings.Contains(got, "no dir2mcp daemon registered") {
		t.Errorf("stdout: want informational no-op message, got %q", got)
	}
}

// TestDown_StalePidFile_CleansUp: a pid file pointing to a dead PID
// should be cleaned up without error and reported as a stale-pid case
// in JSON output.
func TestDown_StalePidFile_CleansUp(t *testing.T) {
	root, stateDir := newDownFixture(t)
	// pid 999999 is essentially never alive on a fresh system; even if
	// it is by some accident, it isn't our daemon. The test asserts the
	// "stale_pid" branch fired and the file was removed.
	const stalePID = 999999
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", stalePID)), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, root, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "down"})
		if code != 0 {
			t.Fatalf("exit code: got=%d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Pid     int    `json:"pid"`
		Stopped bool   `json:"stopped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal stdout: %v raw=%s", err, stdout.String())
	}
	if payload.Reason != "stale_pid" {
		t.Errorf("reason: want stale_pid got %q", payload.Reason)
	}
	if payload.Stopped {
		t.Errorf("stopped: want false (nothing alive to stop) got true")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file should have been removed; stat err=%v", err)
	}
}

// TestDown_MalformedPidFile_RecoversAndReports: a non-integer pid file
// is treated as residue — removed, reported, exit 0.
func TestDown_MalformedPidFile_RecoversAndReports(t *testing.T) {
	root, stateDir := newDownFixture(t)
	pidPath := filepath.Join(stateDir, "server.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, root, func() {
		code := app.RunWithContext(context.Background(), []string{"down"})
		if code != 0 {
			t.Fatalf("exit code: got=%d stderr=%s", code, stderr.String())
		}
	})
	if got := stdout.String(); !strings.Contains(got, "removed malformed pid file") {
		t.Errorf("stdout: want malformed cleanup message, got %q", got)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("malformed pid file should have been removed; stat err=%v", err)
	}
}

// TestDown_RejectsPositionalArguments: the subcommand has no positional
// args; passing one should fail with an actionable error rather than
// silently ignoring it.
func TestDown_RejectsPositionalArguments(t *testing.T) {
	root, _ := newDownFixture(t)
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, root, func() {
		code := app.RunWithContext(context.Background(), []string{"down", "extra"})
		if code == 0 {
			t.Fatalf("expected non-zero exit, got 0; stdout=%s stderr=%s", stdout.String(), stderr.String())
		}
	})
	if got := stderr.String(); !strings.Contains(got, "positional arguments") {
		t.Errorf("expected positional-arg error, got: %q", got)
	}
}

// TestDown_ClearsStaleConnectionFile_ForEveryNonLivePID pins issue #714.
// Each pid classification below proves that no owned live daemon exists. A
// successful `down` must therefore clear both the pid record and the published
// connection.json. A leftover connection.json points clients, bridges, and the
// legacy remote commands at a dead or reassigned endpoint.
func TestDown_ClearsStaleConnectionFile_ForEveryNonLivePID(t *testing.T) {
	const staleConnection = `{"format_version":"0.1.0","url":"http://127.0.0.1:9/mcp"}`

	cases := []struct {
		name string
		// seedPID writes the pid file for this classification. A nil value
		// means "no pid file at all".
		seedPID    func(t *testing.T, pidPath string)
		wantReason string
	}{
		{
			name:       "no_pid_file",
			seedPID:    nil,
			wantReason: "no_pid_file",
		},
		{
			name: "malformed_pid_file",
			seedPID: func(t *testing.T, pidPath string) {
				t.Helper()
				if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o600); err != nil {
					t.Fatalf("seed malformed pid file: %v", err)
				}
			},
			wantReason: "malformed_pid_file",
		},
		{
			name: "dead_pid",
			seedPID: func(t *testing.T, pidPath string) {
				t.Helper()
				if err := os.WriteFile(pidPath, []byte("999999\n"), 0o600); err != nil {
					t.Fatalf("seed stale pid file: %v", err)
				}
			},
			wantReason: "stale_pid",
		},
		{
			name: "recycled_pid",
			seedPID: func(t *testing.T, pidPath string) {
				t.Helper()
				// A live pid (this test process) under a start-time token that
				// does not match: the signature of a recycled pid. The process
				// must stay untouched (#418).
				realToken := requireStartTokens(t)
				if err := cli.WritePIDRecordForTest(pidPath, os.Getpid(), realToken+"-stale"); err != nil {
					t.Fatalf("seed recycled pid file: %v", err)
				}
			},
			wantReason: "recycled_pid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, stateDir := newDownFixture(t)
			pidPath := filepath.Join(stateDir, "server.pid")
			connPath := filepath.Join(stateDir, "connection.json")
			if tc.seedPID != nil {
				tc.seedPID(t, pidPath)
			}
			if err := os.WriteFile(connPath, []byte(staleConnection), 0o600); err != nil {
				t.Fatalf("seed connection file: %v", err)
			}

			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)
			withWorkingDir(t, root, func() {
				code := app.RunWithContext(context.Background(), []string{"--json", "down"})
				if code != 0 {
					t.Fatalf("exit code: got=%d want=0 stderr=%s", code, stderr.String())
				}
			})

			var payload struct {
				Stopped bool   `json:"stopped"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal stdout: %v raw=%s", err, stdout.String())
			}
			if payload.Reason != tc.wantReason {
				t.Errorf("reason: got %q want %q", payload.Reason, tc.wantReason)
			}
			if payload.Stopped {
				t.Errorf("stopped: want false (nothing live to stop) got true")
			}
			if _, err := os.Stat(connPath); !os.IsNotExist(err) {
				t.Errorf("connection.json should have been removed; stat err=%v", err)
			}
			if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
				t.Errorf("pid file should have been removed; stat err=%v", err)
			}
		})
	}
}

// TestDown_IsIdempotentWithoutConnectionFile: `down` must not fail when there
// is no connection.json to remove, so teardown scripts can run twice.
func TestDown_IsIdempotentWithoutConnectionFile(t *testing.T) {
	root, stateDir := newDownFixture(t)
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		withWorkingDir(t, root, func() {
			code := app.RunWithContext(context.Background(), []string{"down"})
			if code != 0 {
				t.Fatalf("run %d exit code: got=%d stderr=%s", i, code, stderr.String())
			}
		})
		if strings.Contains(stderr.String(), "warning") {
			t.Errorf("run %d: unexpected warning: %q", i, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "connection.json")); !os.IsNotExist(err) {
		t.Errorf("connection.json should not exist; stat err=%v", err)
	}
}

// newDownFixture builds a root directory with a .dir2mcp state subdir
// pre-created. The state dir is what the down command operates on.
func newDownFixture(t *testing.T) (root, stateDir string) {
	t.Helper()
	root = t.TempDir()
	stateDir = filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	return root, stateDir
}

// withWorkingDir is shared across CLI tests; defined in up_command_test.go.
