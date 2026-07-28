# dirstral-annotator

The **reference recognition backend** for dir2mcp's `recognize` capability
([dirstral-spec design 0004](../dirstral-spec/docs/design/0004-recognize-capability.md)):
turns game footage + a roster into time-coded, confidence-scored statements
("every pitch by Logan Webb", with timecodes an editor can cut from), which
dir2mcp persists as a derived `recognition` representation and serves
through `search`/`ask` with `time`-span citations.

dir2mcp never runs this package directly — it connects to it:

```
dir2mcp (recognize.provider=serve) ──POST /recognize {"path": …}──▶ dirstral-annotator serve
                                   ◀── annotations JSON (design 0004 §5) ──
```

## The cascade

Recognizers each emit *cues* (time range + player + event + confidence);
fusion merges agreeing cues with noisy-OR confidence. Cheapest and most
reliable first:

| Source | What it reads | Needs |
|---|---|---|
| `playbyplay` | MLB statsapi GUMBO feed: every pitch, official identities, wall-clock timestamps | a `games.json` binding (+ network, or a saved feed) |
| `scorebug` | Broadcast overlay names via OCR | `[ocr]` extra + tesseract |
| `jersey` | Numbers on detected player crops | `[jersey]` extra |
| `face` | Roster image bank vs. sampled frames | `[face]` extra |

Core (play-by-play, fusion, serving, eval) is **stdlib-only**; vision
recognizers pull their stacks via extras and are skipped gracefully when
unavailable.

## Run the backend

```bash
pip install -e .                 # core
pip install -e '.[ocr,face]'     # + scorebug OCR + face recognition

dirstral-annotate serve --roster roster.json --games games.json \
  --scorebug --faces bank/ --min-confidence 0.3 --port 8765
```

Then point dir2mcp at it:

```yaml
# .dir2mcp config
recognize_provider: serve
recognize_serve_url: http://127.0.0.1:8765
```

Or let dir2mcp own the whole lifecycle — launch, health-wait, and shutdown —
so `dir2mcp up` is the only command anyone runs:

```yaml
recognize_provider: serve
recognize_serve_url: http://127.0.0.1:8765
recognize_serve_command: dirstral-annotate serve --roster roster.json --games games.json --port 8765
```

`dir2mcp up` over the footage directory now indexes every video through the
backend; a backend failure is recorded per-document exactly like an STT
failure, and in managed mode a backend that never becomes healthy fails
startup loudly instead.

### Configuration files

`roster.json` — the entity vocabulary:

```json
[{"id": "player:webb-logan", "name": "Logan Webb", "number": "62",
  "aliases": ["Webb", "L. Webb"], "mlbam_id": 657277}]
```

`games.json` — per-video play-by-play binding (optional; vision recognizers
work without it):

```json
{"game7.mp4": {"game_pk": 745123, "anchors": ["1789265400.0=60.0"]}}
```

An **anchor** (`WALL_CLOCK_EPOCH=VIDEO_SECONDS`) places the official
timeline on the video file. One suffices; several detect splices.

Face bank layout: `bank/player_webb-logan/*.jpg` (10–20 images per player;
`:` → `_` in dir names).

## Evaluate (the phase-1 gate)

```bash
dirstral-annotate eval game7.mp4 --roster roster.json \
  --game-pk 745123 --anchor "1789265400.0=60.0" --report report.md
# exit 0 iff recall ≥ 90% and precision ≥ 95% (the pilot commitments)
```

Ground truth comes free from MLB statsapi (per-pitch identities +
timestamps, ~2017 onward) — no manual annotation.

## Tests

```bash
pip install -e '.[test]'   # test runner + jsonschema (external test tooling)
python3 -m pytest tests/    # tested code needs no network, models, or ffmpeg
```

The recognizer/fusion/eval **code** under test is stdlib-only; only the
external test tooling (`pytest`, and `jsonschema` for the contract test) is
installed via the `test` extra. The suite includes a live HTTP round-trip
against the serve handler and a schema-agreement test validating responses
against the dirstral-spec draft wire contract (a hard failure if `jsonschema`
is missing, skipped only when the `dirstral-spec` schema isn't checked out).
The dir2mcp side is pinned by `tests/ingest/recognize_test.go`.
