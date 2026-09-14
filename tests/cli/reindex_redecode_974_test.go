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
	// A whisper endpoint that is never called: the arming step runs BEFORE the
	// rebuild and only needs a transcriber to RESOLVE. `off` is not usable here,
	// because the flag now refuses it (a run with no transcriber decodes nothing
	// and would report a repair it cannot perform).
	cfgBody := "stt:\n  provider: \"whisper\"\nproviders:\n  whisper:\n    kind: whisper\n    base_url: \"http://127.0.0.1:9/v1\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr strings.Builder
	testutil.WithWorkingDir(t, dir, func() {
		app := cli.NewAppWithIO(&stdout, &stderr)
		_ = app.Run(append([]string{"--non-interactive", "reindex"}, extra...))
	})
	return stderr.String()
}

func TestReindexRedecode_CountsRecordingsNotTranscripts(t *testing.T) {
	// §8.6.12 gives every additional audio track its own transcript, so one
	// recording can hold several partial transcripts. The report counts those
	// separately, because each is its own decode with its own missing audio, but
	// the repair is per DOCUMENT: announcing "2 recordings" for one file would
	// misreport what the run is about to do.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/dual.mp4", repType: "transcript",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, 10*minute, 73*minute)},
		seedRep{relPath: "rfe/dual.mp4", repType: "transcript@t1",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 2, 20*minute, 73*minute)},
	)
	_ = st.Close()

	stderr := runReindexIn(t, dir, "--redecode-partial-transcripts")
	if !strings.Contains(stderr, "re-decoding 1 recording(s)") {
		t.Errorf("two tracks of one recording must count as one recording: %q", stderr)
	}
}

func TestReindexRedecode_RefusesWhenNoTranscriberResolves(t *testing.T) {
	// With stt.provider off, transcription is skipped entirely: the armed paths
	// are never decoded and never written back, Reindex returns success, and the
	// cache still holds the partial text. The operator would read a green run as
	// a repair, which is the exact failure this flag exists to remove.
	//
	// TranscriberFromConfig returns (nil, nil) for `off`, not an error, so the
	// nil is the case that has to be caught.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/one.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, 10*minute, 73*minute)})
	_ = st.Close()

	cfgPath := filepath.Join(dir, ".dir2mcp.yaml")
	if err := os.WriteFile(cfgPath, []byte("stt:\n  provider: \"off\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr strings.Builder
	var code int
	testutil.WithWorkingDir(t, dir, func() {
		app := cli.NewAppWithIO(&stdout, &stderr)
		code = app.Run([]string{"--non-interactive", "reindex", "--redecode-partial-transcripts"})
	})
	if code == 0 {
		t.Errorf("exit = 0, want a configuration error: a run that decodes nothing must not look like a repair")
	}
	if !strings.Contains(stderr.String(), "needs a working speech-to-text provider") {
		t.Errorf("the refusal does not say what is missing: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "re-decoding") {
		t.Errorf("the run announced a repair it cannot perform: %q", stderr.String())
	}
}
