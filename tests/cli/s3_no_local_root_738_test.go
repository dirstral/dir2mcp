package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// s3ConfigYAML renders a minimal, valid native-S3 corpus config whose corpus
// root points at a local directory that does NOT exist. Per SPEC §7.8 the
// bucket+prefix IS the corpus root for source.kind=s3 (S3FS.Walk ignores its
// root argument), so this is a legitimate remote-only deployment: only the
// state directory has to be local.
func s3ConfigYAML(rootDir, stateDir string) string {
	return "root_dir: " + rootDir + "\n" +
		"state_dir: " + stateDir + "\n" +
		"source:\n" +
		"  kind: s3\n" +
		"  s3:\n" +
		"    bucket: my-corpus-bucket\n" +
		"    prefix: corpus/\n"
}

// writeConfig writes cfg to <dir>/.dir2mcp.yaml.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// prepareS3UpEnv clears every provider credential (so the run stops at the
// embed preflight, which is the FIRST check after root validation) and supplies
// the AWS credentials source.kind=s3 requires at runtime.
func prepareS3UpEnv(t *testing.T) {
	t.Helper()
	clearProviderEnv(t)
	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
}

// Issue #738: `up` on a native-S3 corpus must not demand a local corpus root.
// Before the fix validateUpConfig called ensureRootAccessible(cfg.RootDir)
// unconditionally, so a healthy remote-only deployment exited with
// "root inaccessible" before S3 was ever contacted.
//
// The assertion is bounded: with no embedding provider configured the run must
// reach the embed preflight (the next check after root validation) and fail
// there instead.
func TestUpS3SourceDoesNotRequireLocalRoot_738(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	missingRoot := filepath.Join(tmp, "does-not-exist")
	writeConfig(t, tmp, s3ConfigYAML(missingRoot, stateDir))
	prepareS3UpEnv(t)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--foreground"})
	})
	if code == 0 {
		t.Fatalf("expected a non-zero exit from the embed preflight, stderr=%s", stderr.String())
	}
	out := stderr.String()
	if strings.Contains(out, "root inaccessible") {
		t.Fatalf("s3 source must not require a local corpus root (#738); got: %s", out)
	}
	if !strings.Contains(out, "no embedding provider configured") {
		t.Fatalf("expected startup to reach the embed preflight; got: %s", out)
	}
}

// The local-corpus contract is unchanged: a missing root_dir is still a hard
// startup failure for the default source.kind.
func TestUpLocalSourceStillRequiresLocalRoot_738(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	missingRoot := filepath.Join(tmp, "does-not-exist")
	writeConfig(t, tmp, "root_dir: "+missingRoot+"\nstate_dir: "+stateDir+"\n")
	clearProviderEnv(t)
	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--foreground"})
	})
	if code == 0 {
		t.Fatalf("a missing local corpus root must fail, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "root inaccessible") {
		t.Fatalf("local source must still require an accessible root; got: %s", stderr.String())
	}
}

// nfs is a mounted filesystem path, so it keeps the local-root requirement too.
func TestUpNFSSourceStillRequiresLocalRoot_738(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	missingRoot := filepath.Join(tmp, "does-not-exist")
	writeConfig(t, tmp,
		"root_dir: "+missingRoot+"\nstate_dir: "+stateDir+"\nsource:\n  kind: nfs\n")
	clearProviderEnv(t)
	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--foreground"})
	})
	if code == 0 {
		t.Fatalf("a missing nfs mount root must fail, stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "root inaccessible") {
		t.Fatalf("nfs source must still require an accessible root; got: %s", stderr.String())
	}
}
