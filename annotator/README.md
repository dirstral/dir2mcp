# dirstral-annotator (pilot)

External recognition pipeline for the video-intelligence pilot: turns game
footage + a roster into **time-coded annotation sidecars** that dir2mcp
indexes into searchable, citable moments ("every pitch by Logan Webb", with
timecodes an editor can cut from).

This is deliberately **not** part of the dir2mcp Go core. dir2mcp indexes
what this package publishes; it never runs it. The contract is the sidecar
file format — see
[dirstral-spec design 0004](../dirstral-spec/docs/design/0004-annotation-sidecars.md):

- `game7.vtt` — the **v0** WebVTT convention. Indexed by dir2mcp **today**
  via the existing subtitle-sidecar mechanism (SPEC §8.6.4); each cue
  becomes a BM25+vector-searchable chunk citing a `time` span.
- `game7.annotations.json` — the **v1 draft** machine-readable format
  (entities, confidence, annotator provenance). Ignored by dir2mcp until
  design 0004 v1 lands; emitted now so the pilot corpus is forward-compatible.

## The cascade

Recognizers each emit *cues* (time range + player + event + confidence);
fusion merges agreeing cues with noisy-OR confidence. Cheapest and most
reliable first:

| Source | What it reads | Needs |
|---|---|---|
| `playbyplay` | MLB statsapi GUMBO feed: every pitch, official identities, wall-clock timestamps | network (or a saved feed) + 1 time anchor |
| `scorebug` | Broadcast overlay names via OCR | `[ocr]` extra + tesseract |
| `jersey` | Numbers on detected player crops | `[jersey]` extra |
| `face` | Roster image bank vs. sampled frames | `[face]` extra |

Core (play-by-play, fusion, emitters, eval) is **stdlib-only**; vision
recognizers pull their stacks via extras and are skipped gracefully when
unavailable.

## Install & use

```bash
pip install -e .                 # core
pip install -e '.[ocr,face]'     # + scorebug OCR + face recognition

# Annotate: writes game7.vtt + game7.annotations.json next to the media
dirstral-annotate annotate game7.mp4 --roster roster.json \
  --game-pk 745123 --anchor "1789265400.0=60.0" \
  --scorebug --faces bank/

# Evaluate against Statcast ground truth (the phase-1 gate):
dirstral-annotate eval game7.mp4 --roster roster.json \
  --game-pk 745123 --anchor "1789265400.0=60.0" --report report.md
# exit 0 iff recall ≥ 90% and precision ≥ 95% (the pilot commitments)
```

An **anchor** (`WALL_CLOCK_EPOCH=VIDEO_SECONDS`) places the official
timeline on your video file. One suffices; several detect splices (the CLI
warns when anchors disagree by >5 s).

`roster.json`:

```json
[{"id": "player:webb-logan", "name": "Logan Webb", "number": "62",
  "aliases": ["Webb", "L. Webb"], "mlbam_id": 657277}]
```

Face bank layout: `bank/player_webb-logan/*.jpg` (10–20 images per player;
`:` → `_` in dir names).

## Tests

```bash
python3 -m pytest tests/   # stdlib-only; no network, models, or ffmpeg
```

## Indexing the output with dir2mcp

Point `dir2mcp up` at the footage directory after annotating: the `.vtt`
sidecars are picked up by the standard ingest path — no configuration
needed. The covered behavior (annotation-convention cues become time-span
chunks) is pinned by `tests/ingest/annotation_sidecar_test.go`.
