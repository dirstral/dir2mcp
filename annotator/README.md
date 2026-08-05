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

### The overlay text reader

`recognizers/overlay.py` is the reusable half of the scorebug recognizer:
find the band of the frame that holds burned-in text, defeat light-on-dark
with a second hard-thresholded pass, OCR both, and hand back
`(timestamp, region, texts)`. It knows nothing about sport or roster.
`ScorebugRecognizer` is one consumer of it (baseball fields against a roster);
a news archive is another (headline banners, tickers, a burned-in clock badge
as a wall-clock anchor), and gets there with different `regions=` and a
different interpretation rather than different reading code.

The interpretation is passed in, and it also tells the reader how much
evidence each band held: that hit count is what the band search locks onto,
and it has to be the caller's judgement, because "the OCR returned something"
is true of a crop of crowd texture too.

OCR language is configurable, never a constant in the code:

```bash
DIRSTRAL_ANNOTATOR_OCR_LANG=rus dirstral-annotate serve …   # or lang="rus"
```

Non-Latin scripts need the matching tesseract traineddata installed
(`brew install tesseract-lang`, `apt install tesseract-ocr-rus`).

### The burned-in clock reader

`recognizers/clock.py` is one consumer of that reader, and the one that pays
for itself fastest on an archive: a video timestamp is only citable once
something says what wall-clock instant it was, and a news broadcast burns that
into the corner of the frame.

```python
from datetime import date
from pathlib import Path

from dirstral_annotator.recognizers.clock import ClockReader

reader = ClockReader(zones={"MSK": "Europe/Moscow"}, anchor_date=date(2025, 8, 20))
anchor = reader.anchor(Path("broadcast.mp4"))   # None when there is no badge
if anchor is not None:
    anchor.wall_clock_at(1830.0)                # aware datetime of video 00:30:30
```

What it will not do is guess. The zone table is required (a badge label the
caller has not named is not a reading), the date is required for an epoch (a
badge shows a time of day and no date), a single reading is never an anchor,
and readings that imply two different anchors are reported as two segments
rather than averaged into one that is wrong for both.

Two settings are measured rather than stylistic, and both differ from the
scorebug's:

- **psm 11, not 6.** A badge is an island of text in an empty corner, not a
  dense strip. On sample footage psm 6 never read the badge at any crop size;
  psm 11 read it.
- **The OCR language changes the digits.** On the same pixels, a badge showing
  `21:35` read as `21:35` under `eng` and as `21:55` under `rus`. Neither
  reading is detectably wrong on its own, and a 20-minute error in an anchor is
  inherited by every citation under it. Do not hand the clock reader the
  language you use for the prose overlays without checking it against a frame,
  and list the zone label the way your OCR actually renders it (a Latin-script
  model returns `MCK` for Cyrillic `МСК`, so a Russian corpus may want both
  spellings in the table).

### The news overlay interpreter

`recognizers/news.py` is the corpus-side counterpart of the scorebug: same
reader, different idea of what the text MEANS. Baseball resolves it against a
roster; a news broadcast has no roster and no feed, so the words are the
payload.

```python
from pathlib import Path

from dirstral_annotator.recognizers.news import NewsOverlayRecognizer

cues = NewsOverlayRecognizer(lang="rus").recognize(Path("broadcast.mp4"))
# [Cue(source='news', event='headline', start_s=0.0, end_s=90.0,
#      text='«ЯНДЕКС» ОШТРАФОВАЛИ ЗА ОТКАЗ ПРЕДОСТАВИТЬ ФСБ ДОСТУП К «АЛИСЕ»'), ...]
```

The interpreter has no vocabulary to check against, so what it counts as
evidence is **agreement between the two preprocessing passes**. Real glyphs
survive both renderings; noise is a property of the rendering and does not.
Measured on 105 band reads of a TV Rain broadcast, comparing the two overlay
bands against five background bands per frame:

| population | median agreement | worst |
|---|---|---|
| overlay (30 reads) | 0.70 | 0.20 |
| background (75 reads) | 0.00 | 0.00 |

No background band agreed at all, so the default floor of 0.3 keeps 93% of
overlay reads and admits none of the 75 background ones. It is a parameter
regardless: one corpus is one corpus.

A news frame carries several overlays at once, so each role gets its own reader
over its own bands (the clock badge is `clock.py`, which has much stronger
evidence in a parseable time next to a named zone). Frame extraction is shared,
so a second role costs OCR on a second band, not a second decode.

**Known limitation.** Ticker cues can carry a garbage prefix, and on the 90s
sample one cue of ten is garbage throughout. It is stable across both passes,
so it is real pixels being misread rather than noise the agreement floor can
reject. A token-level cleaner was built and measured against this footage and
is NOT shipped: it dropped real content (`220` from `около 220 тысяч`, the
preposition `К`) while missing the actual garbage, which has perfectly ordinary
character statistics.

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
