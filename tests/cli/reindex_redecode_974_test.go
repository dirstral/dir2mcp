package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// --redecode-partial-transcripts is the action the §7.7 remediation names
// (#974). Without it a partial transcript is permanent: the cache is keyed on
// the media bytes and the STT derivation identity, and repairing an endpoint
// changes neither, so an ordinary reindex restores the same partial text.

func TestReindexRedecode_RejectsTheCombinationThatCannotWork(t *testing.T) {
	// --embeddings-only re-queues chunks a provider rejected and runs no
	// extraction, so it can never reach a transcript. Accepting the pair would
	// let an operator believe they had repaired their transcripts by running
	// something that cannot.
	var stderr strings.Builder
	app := cli.NewAppWithIO(&strings.Builder{}, &stderr)

	if code := app.Run([]string{"reindex", "--embeddings-only", "--redecode-partial-transcripts"}); code == 0 {
		t.Fatalf("the combination must be refused, got exit 0")
	}
	if !strings.Contains(stderr.String(), "runs no transcription") {
		t.Errorf("the refusal does not say why: %q", stderr.String())
	}
}

func TestReindexRedecode_SaysSoWhenThereIsNothingToRepair(t *testing.T) {
	// Said positively, for the same reason the doctor check states a clean
	// corpus: silence here reads as "it worked" when the truth is "there was
	// nothing to do", and the operator has just repaired an endpoint and wants
	// to know whether the repair reached anything.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/complete.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)})
	_ = st.Close()

	stderr := runReindexIn(t, dir, "--redecode-partial-transcripts")
	if !strings.Contains(stderr, "nothing to re-decode") {
		t.Errorf("a clean corpus was not reported positively: %q", stderr)
	}
}

func TestReindexRedecode_NamesHowManyItWillRepair(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/one.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, 10*minute, 73*minute)},
		seedRep{relPath: "rfe/two.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 2, 20*minute, 73*minute)},
		// Complete: must not be re-decoded, because re-decoding a corpus that is
		// mostly fine is the cost this flag exists to avoid.
		seedRep{relPath: "rfe/fine.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)},
	)
	_ = st.Close()

	stderr := runReindexIn(t, dir, "--redecode-partial-transcripts")
	if !strings.Contains(stderr, "re-decoding 2 recording(s)") {
		t.Errorf("the run does not name what it will repair: %q", stderr)
	}
}

func TestReindexRedecode_APlainReindexAnnouncesNothing(t *testing.T) {
	// The ordinary reindex stays the cheap operation.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/one.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, 10*minute, 73*minute)})
	_ = st.Close()

	stderr := runReindexIn(t, dir)
	if strings.Contains(stderr, "re-decoding") {
		t.Errorf("a plain reindex opted into a re-decode: %q", stderr)
	}
}

// runReindexIn runs `dir2mcp --non-interactive reindex <extra...>` in dir and
// returns stderr. The exit code is not asserted: these tests are about what the
// run SAYS it will repair, which is decided before the rebuild runs.
//
// STT is switched off in the corpus config, because the arming step runs AFTER
// the ingestor is built and an unresolvable provider credential would fail the
// run before it could say anything.
func runReindexIn(t *testing.T, dir string, extra ...string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, ".dir2mcp.yaml")
	if err := os.WriteFile(cfgPath, []byte("stt:\n  provider: \"off\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr strings.Builder
	testutil.WithWorkingDir(t, dir, func() {
		app := cli.NewAppWithIO(&stdout, &stderr)
		_ = app.Run(append([]string{"--non-interactive", "reindex"}, extra...))
	})
	return stderr.String()
}
