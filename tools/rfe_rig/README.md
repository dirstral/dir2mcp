# RFE validation rig (dir2mcp #1032)

A repeatable measurement instrument for the RFE multilingual validation
corpus (`/mnt/data/rfe-val` on q2e). It replaces the one-off scripts behind
#566 (cross-model agreement as a correctness proxy) and #964 (answer-language
fidelity) with one CLI. The small summary tables of each run are tracked
under `baseline/<date>/`; the full decode outputs (transcripts with word
timings, per-window agreement rows) stay on q2e under
`/mnt/data/rfe-val/rig/results/<date>/` and are git-ignored here.

Pure Python 3 standard library. The two local decoders (MMS, faster-whisper)
run in separate interpreters that carry torch and CTranslate2; the rig itself
never imports them.

## Commands

```
python3 -m tools.rfe_rig [--results DIR] <command> ...
```

`--results` defaults to `tools/rfe_rig/results/<YYYY-MM-DD>` (git-ignored).

| command | what it does |
|---|---|
| `transcribe --decoder SPEC REC...` | decode recordings into the transcript cache (`results/transcripts/<rec>/<decoder_id>.json`); the cache key is (recording, decoder name, decoder version), so a re-run with the same decoder is a hit and a new model revision or a re-indexed state is a miss |
| `agreement --recording REC --a PREFIX --b PREFIX` | compare two cached decoders on a fixed 30 s grid; writes `results/agreement/<rec>__<a>__vs__<b>.json` and prints the worst windows |
| `coverage --state-dir DIR [--media-dir DIR]` | read the dir2mcp state sqlite (copied first, opened read-only) for the per-recording baseline |
| `ask [--runs N] [--token-file F]` | the #964 measurement against a running daemon; `ask --replay FILE` re-scores a stored answer file without a daemon |
| `report [--summary-dir DIR]` | `summary.md`, `summary.json` and `agreement.csv` over a results directory, also copied to `--summary-dir` |

Decoder specs:

* `http://HOST:PORT?model=NAME[&name=LABEL]`: an OpenAI-compatible
  `POST /v1/audio/transcriptions` server (verbose_json). The audio is converted
  to 16 kHz mono WAV first; the version comes from `GET /health`.
* `sqlite:STATE_DIR`: the transcript dir2mcp already delivered, read from the
  state sqlite (live chunks and their word timings). Nothing is decoded. The
  version is the representation's model and `rep_hash`.
* `mms:ADAPTER`: `facebook/mms-1b-all` on CPU via `mms_decode.py`
  (`--mms-python` names the torch interpreter). The version is the adapter,
  the cached checkpoint revision and the transformers version.
* `fw:MODEL_DIR?language=xx`: a local CTranslate2 whisper model on CPU via
  `fw_decode.py` (`--fw-python` names the faster-whisper interpreter).

The MCP bearer token is read from `--token-file` or `RFE_RIG_TOKEN_FILE`. It
is never printed and never written into a result.

## Metrics

**Normalisation** (`textnorm.py`): NFKC, lowercase, punctuation to spaces,
apostrophes dropped inside words, `ё` folded to `е`. Digits stay as tokens, so
"2005" against "эки миң бешинчи" counts as a disagreement.

**Windowing** (`windows.py`): both transcripts are re-binned onto a 30 s grid
anchored at 0 s of the recording. A token goes to the window that contains its
word start time. A segment without word timings has its tokens spread across
the segment in proportion to token length, then binned. The issue text said
"all segments overlapping the window"; that was rejected because dir2mcp's
delivered chunks are about 34 s long, so every 30 s window would collect two
chunks (60 s of text) against 30 s from the other decoder.

**Per window** (`agreement.py`): `wer` = token edit distance / decoder A
tokens (A is the reference); `ned` = edit distance / max(A, B) tokens,
symmetric and bounded in [0, 1]; `speech` = at least one decoder has a token;
`agree` = speech and `wer <= 0.5`.

**Summary**: `agree_fraction_all` = agreeing / all windows;
`coverage_by_agreement` = agreeing / speech windows; `mean_wer`, `mean_ned`
over speech windows; `overall_wer` = sum of edits / sum of A tokens. The ten
worst windows by `ned` are listed in each agreement file.

**Coverage baseline** (`coverage.py`), per transcript representation:
`language`, `language_source`, `language_confidence`, the SPEC 8.6.13
`coverage` object, `language_covered` (both `absent` on the current state:
this is what #1029/#1030 will populate), live and deleted chunk counts, live
chunk window lengths, and two duration coverages:

* `delivered_coverage`: union of live chunk time spans / recording duration.
  The share of the audio an editor can search.
* `deleted_coverage`: the same over the chunks retired by #955's re-chunking
  (the 7.5 s chunks still sit in the sqlite with `deleted=1`). This is the
  regime the #566 "27%" was measured in.

**Answer language** (`ask.py`, `detect.py`): `detect()` is ported unchanged
from `/mnt/data/rfe-val/detect.py`, the word-and-letter voting detector that
scored the #964 run. `tags` counts `[file@t=m:ss]` citation tags in the
answer; `citations_n` counts the structured citations the tool returned. An
answer of the daemon's retrieved-context fallback shape
(`Question: ... Top context: ...`) is flagged `generated=false`: its language
is that of the retrieved chunks, not of a model.

### Relation to the #566 numbers

* "27% coverage" in #566 was the delivered share of a 73-minute Kyrgyz
  recording (pilot item 18876, not in this corpus) under 7.5 s chunking. On
  the validation corpus the same quantity is `deleted_coverage` below.
* "about 15% WER" in #566 was whisper-small-kyrgyz against MMS on six
  hand-picked clean clips. The rig scores every 30 s window of the whole
  recording with the same token normalisation; see the agreement rows for
  `whisper-small-kyrgyz-ct2` vs `mms-1b-all-kir`. MMS is CTC and runs words
  together, and a 30 s hard chunk cuts a word at each boundary; both count as
  errors here and did not on the clean clips.

## Tests

```
python3 -m unittest discover -s tools/rfe_rig/tests
```

`tests/test_ask_replay.py` replays `fixtures/ask_2026-09-12_main-final.json`
(the measure.py output that #964 quoted, 15 KB of questions and answers) and
asserts 24 of 24. This is the regression guard the issue asked for: it holds
without a daemon.

## Baseline run 2026-09-22

Summaries: `baseline/2026-09-22/{summary.md,summary.json,agreement.csv}`.
Full outputs on q2e: `/mnt/data/rfe-val/rig/results/2026-09-22/` (8.2 MB:
`transcripts/<recording>/<decoder_id>.json` with word timings,
`agreement/*.json` with every window's text and scores, `ask.json`,
`ask_replay.json`, `coverage.json`).

Whisper large-v3-multi output was **extracted from the dir2mcp state sqlite**
(`sqlite:` decoder) rather than re-decoded on the shared GPU: the state
carries a `transcript` representation with `model=large-v3-multi` and
per-word time spans for all 8 recordings. GigaAM-v3 ran on the already
running server at `:9002` (GPU, one recording at a time). MMS and the Kyrgyz
specialist ran on CPU (`nice`, 10 and 4 threads; MMS decoded the whole corpus
in 71 minutes, about 3.9x realtime).

Commands, in order (paths as on q2e, run from `/mnt/data/rfe-val/rig` with
`tools/` rsynced there):

```
R=/mnt/data/rfe-val/rig/results/2026-09-22
C=/mnt/data/rfe-val/corpus_live
RFE_RIG_TOKEN_FILE=/mnt/data/rfe-val/retrieval/.dir2mcp/secret.token \
  python3 -m tools.rfe_rig --results $R ask --runs 2
python3 -m tools.rfe_rig --results $R ask --replay tools/rfe_rig/fixtures/ask_2026-09-12_main-final.json
python3 -m tools.rfe_rig --results $R coverage --state-dir /mnt/data/rfe-val/retrieval/.dir2mcp --media-dir $C
python3 -m tools.rfe_rig --results $R transcribe --decoder sqlite:/mnt/data/rfe-val/retrieval/.dir2mcp $C/*
python3 -m tools.rfe_rig --results $R transcribe --decoder "http://127.0.0.1:9002?name=gigaam-v3-e2e_rnnt" \
  $C/rus_28974_shalygina_interview.flac $C/geo_2007_kikabidze_interview.flac $C/ukr_1108_kravchuk_interview.flac
tools/rfe_rig/scripts/mms_prefetch_loop.sh   # facebook/mms-1b-all into HF_HOME=/mnt/data/rfe-val/hf (3.7 GB)
tools/rfe_rig/scripts/mms_run.sh             # mms:kir / mms:ukr / mms:rus by speech language, CPU
tools/rfe_rig/scripts/fw_run.sh              # whisper-small-kyrgyz CT2, CPU, language=kk
tools/rfe_rig/scripts/agreement_all.sh $R $C # the 20 pairs below
python3 -m tools.rfe_rig --results $R report --summary-dir /mnt/data/rfe-val/rig/baseline/2026-09-22
```

### Answer language

| measurement | result |
|---|---|
| live daemon, 12 questions x 2 runs | **20 of 24** in the language asked; **0 of 24** with a citation tag; **24 of 24 were retrieved-context fallbacks** |
| replay of the stored 2026-09-12 answers | **24 of 24**; 20 of 24 with a citation tag (the 4 untagged are "the context does not contain this" answers) |

The live run does not reproduce #964 because the daemon's generator is down:
`server.log` shows `generator error ... OPENAI_RATE_LIMIT: insufficient_quota`
for every question, and each answer came back in about 2.5 s as
`Question: ... Top context: ...`. The 20 of 24 is therefore the language of
the retrieved chunks: each fallback concatenates the top 15 snippets across
six recordings, mostly Russian, so the detector reads the two English Kuleba
answers and the two Kyrgyz Japarov answers as Russian. The rig flags this in
`ask.json` (`generated: false` on every row, `fallback_answers: 24`,
`cited_structured: 24`). The replay shows the ported detector scores the #964
answers 24 of 24; the measure.py character-only detector scored the same file
21 of 24. Restoring the provider quota is an operator action outside this PR.

### Coverage baseline (dir2mcp state, whisper large-v3-multi)

| recording | speech | detected language | conf | 8.6.13 coverage | language_covered | live chunks | window mean s | delivered % | pre-#955 delivered % |
|---|---|---|---|---|---|---|---|---|---|
| geo_2007_kikabidze_interview.flac | ru | ru | 1.00 | absent | absent | 46 | 33.3 | 99.1 | 78.6 |
| kgz_18448_extremism_hospital.flac | ky | (none) | - | absent | absent | 50 | 36.7 | 97.9 | 83.8 |
| kgz_21138_japarov_broll.flac | ky | mk | 1.00 | absent | absent | 126 | 34.7 | 99.7 | 81.0 |
| kgz_29216_rahat_interview.wav | ky | tk | 1.00 | absent | absent | 55 | 33.6 | 99.9 | 81.9 |
| rus_28974_shalygina_interview.flac | ru | ru | 0.74 | absent | absent | 50 | 34.6 | 99.8 | 81.1 |
| ukr_0652_filaret_programme.flac | uk | uk | 1.00 | absent | absent | 60 | 33.1 | 99.3 | 69.4 |
| ukr_1108_kravchuk_interview.flac | ru | ru | 1.00 | absent | absent | 55 | 33.4 | 99.8 | 81.7 |
| ukr_18126_kuleba_interview.flac | en+uk | en | 1.00 | absent | absent | 45 | 36.0 | 99.9 | 75.1 |

"speech" is what the recording contains (read from the transcripts; the file
prefix is the RFE service, not the language). Whisper mislabels all three
Kyrgyz recordings (Macedonian, Turkmen, none) with confidence 1.0, which is
the #566 failure the P3 work targets. Duration coverage is near 100% for
every recording under 34 s chunking, Kyrgyz included: Kyrgyz text exists in
the index, it is just not Kyrgyz. Duration coverage is therefore not a
correctness signal on this corpus; agreement is.

### Cross-model agreement (30 s windows, agree = WER <= 0.5)

`whisper` = dir2mcp's delivered whisper large-v3-multi; `gigaam` = GigaAM-v3
e2e_rnnt; `mms-xx` = facebook/mms-1b-all with that adapter; `specialist` =
UlutSoftLLC/whisper-small-kyrgyz (CT2, `language=kk`, the token slot the
fine-tune emits). Full rows in `baseline/2026-09-22/agreement.csv`.

| recording | speech | A | B | windows | agreeing | coverage by agreement | mean WER | overall WER |
|---|---|---|---|---|---|---|---|---|
| rus_28974_shalygina | ru | whisper | gigaam | 58 | 49 | 84.5% | 0.370 | 0.295 |
| rus_28974_shalygina | ru | whisper | mms-rus | 58 | 22 | 37.9% | 0.611 | 0.560 |
| rus_28974_shalygina | ru | gigaam | mms-rus | 58 | 22 | 37.9% | 0.589 | 0.542 |
| geo_2007_kikabidze | ru | whisper | gigaam | 52 | 49 | 94.2% | 0.255 | 0.236 |
| geo_2007_kikabidze | ru | whisper | mms-rus | 52 | 26 | 50.0% | 0.592 | 0.574 |
| geo_2007_kikabidze | ru | gigaam | mms-rus | 52 | 27 | 51.9% | 0.554 | 0.548 |
| ukr_1108_kravchuk | ru | whisper | gigaam | 62 | 61 | 98.4% | 0.208 | 0.200 |
| ukr_1108_kravchuk | ru | whisper | mms-rus | 62 | 47 | 75.8% | 0.417 | 0.420 |
| ukr_1108_kravchuk | ru | gigaam | mms-rus | 62 | 46 | 74.2% | 0.440 | 0.447 |
| ukr_0652_filaret | uk | whisper | mms-ukr | 67 | 64 | 95.5% | 0.274 | 0.270 |
| ukr_18126_kuleba | en+uk | whisper | mms-ukr | 55 | 34 | 61.8% | 0.556 | 0.581 |
| kgz_29216_rahat | ky | whisper | mms-kir | 62 | 0 | 0.0% | 1.110 | 0.960 |
| kgz_29216_rahat | ky | whisper | specialist | 62 | 0 | 0.0% | 1.244 | 1.048 |
| kgz_29216_rahat | ky | specialist | mms-kir | 62 | 21 | 33.9% | 0.582 | 0.576 |
| kgz_18448_extremism | ky | whisper | mms-kir | 63 | 0 | 0.0% | 1.095 | 0.994 |
| kgz_18448_extremism | ky | whisper | specialist | 63 | 0 | 0.0% | 1.173 | 1.084 |
| kgz_18448_extremism | ky | specialist | mms-kir | 63 | 18 | 28.6% | 0.603 | 0.582 |
| kgz_21138_japarov | ky | whisper | mms-kir | 147 | 0 | 0.0% | 1.709 | 1.084 |
| kgz_21138_japarov | ky | whisper | specialist | 147 | 0 | 0.0% | 2.451 | 1.202 |
| kgz_21138_japarov | ky | specialist | mms-kir | 147 | 63 | 42.9% | 0.759 | 0.547 |

What the table says:

* Whisper's Kyrgyz output agrees with neither Kyrgyz-capable decoder on a
  single 30 s window of 272 (mean NED 0.90 to 0.94): the #566 finding,
  measured over 2 h 15 min rather than clips. Coverage by agreement for
  Kyrgyz under the current STT is 0%.
* The two Kyrgyz-capable decoders agree with each other on 29% to 43% of
  windows at mean WER 0.58 to 0.76. That is the proxy's ceiling for Kyrgyz
  with today's models, and it is far from the 15% of #566's six clean clips.
  The worst windows listed in each agreement file are where a native-speaker
  spot check should start.
* On Russian speech whisper and GigaAM agree on 84% to 98% of windows; MMS
  `rus` agrees with both of them equally poorly (38% to 76%), so MMS is the
  outlier there and the whisper transcript is the one to trust.
* On Ukrainian speech whisper and MMS `ukr` agree on 96% of windows
  (Filaret); the Kuleba recording is half English interpretation, which MMS
  `ukr` cannot transcribe, hence 62%.

### Skipped, and why

* Whisper large-v3-multi was not re-decoded over HTTP on `:9010`: the sqlite
  already holds that model's output with word timings, so the GPU shared with
  the pilot was not used for it. The HTTP decoder path was exercised through
  GigaAM on `:9002`.
* MMS `kat` (Georgian) was downloaded but not run: the `geo_` recording is a
  Russian-language interview, so it was decoded with `rus`.
* The native-speaker spot-check set (30 clips per language with human labels)
  is not part of this PR: `/mnt/data/rfe-val` carries no reference
  transcripts (checked 2026-09-22: the media, the dir2mcp state, the
  #955/#964 measurement files and a `batch-cal/` directory of further media,
  nothing else), so there is nothing to calibrate against yet. The agreement
  files' worst windows are the sampling frame for building it.
