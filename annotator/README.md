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

### What the play-by-play recognizer emits

One pitch produces up to seven cues. Each cue carries one fact:

| `event` | Keyed on | Emitted for |
|---|---|---|
| `pitch` | the pitcher, plus the fielding club | every pitch a rostered pitcher threw |
| `at_bat` | the batter, plus the club at the plate | every pitch a rostered batter faced |
| the feed's `result.eventType`: `home_run`, `strikeout`, `walk`, `single`, `double`, `field_out`, ... | the batter, plus the club at the plate | the pitch that ended the at-bat |
| `batted_ball` | the batter, plus the club at the plate | a pitch the tracking measured (`playEvents[].hitData`) |
| `captivating` | the batter, plus the club at the plate | a play the feed scores above 0 on `about.captivatingIndex` |
| `reviewed` | the batter, plus the club at the plate | a play whose call went to a review (`about.hasReview`) |
| `scoring_play` | the batter, plus the club at the plate | a play that scored a run (`about.isScoringPlay`) |

The third cue makes the outcome of a play structured data. Before it, the
outcome was prose inside the cue text alone, so `dir2mcp_search`'s `events`
filter could not reach it. "Who hit home runs? List every player" fell back to
top-k semantic search over that prose, and it answered a "list every X"
question from a partial sample: it named a player who never homered and it
dropped one who did. The vocabulary is MLB's own, so a client selects the real
thing with `events: ["home_run"]` instead of a match on words.

The last four cues answer the questions the outcome cannot: "the most
captivating moments", "the hardest hit ball", "the longest home run", "the
contested calls". Every value is the feed's own, carried verbatim.

A number is neither an event nor an entity, and the design 0004 wire schema
closes `annotations[]` (`additionalProperties: false`), so it has no numeric
field. Each fact therefore reaches a client by the two channels the contract
does have: `event` is the token to FILTER on, and `text` carries the number
itself, in the feed's unit, inside the sentence that gets indexed, which is what
a client RANKS on.

```
Captivating moment (captivating index 95): Bryce Eldridge vs Mitchell Parker
  (bottom of the 9th) — Bryce Eldridge hits a grand slam (4) to right field. …
Batted ball: Matt Chapman vs Foster Griffin (bottom of the 6th): exit velocity
  107 mph, launch angle 22 degrees, distance 421 ft, fly ball, medium contact
  — Matt Chapman homers (5) on a fly ball to left center field.
```

The recognizer sets no threshold on `captivatingIndex`. The one boundary the
feed itself draws is zero (43 of the pilot game's 84 plays score exactly 0), so
`captivating` means "the feed scored this play above zero" and the score rides
in the text. A sharper cut would be this backend's judgement baked into the
index, where no client could undo it.

All the cues of one pitch share one time span, so the answer path groups them
into one moment (issue #784) and counts one event once. The phase-1 metric reads
`event == "pitch"` alone, so none of the added cues move it.

### The overlay text reader

`recognizers/overlay.py` is the reusable half of the scorebug recognizer:
find the band of the frame that holds burned-in text, defeat a busy background
with a second hard-thresholded pass, OCR both, and hand back
`(timestamp, region, texts)`. It knows nothing about sport or roster.

Those two passes span both polarities: the hard cut keeps `p > 140`, so dark ink
on a light panel arrives as black text on white already. What they lose is a
**semi-transparent** light panel over a busy scene, because a light panel at low
opacity is only locally light and no single cut fits the whole band. A band
neither pass could read is therefore re-read once on a local-mean threshold. It
is a fallback and not a third pass: OCR dominates the run, so a band that
already read costs nothing extra, and a run the rendering never helps stops
paying for it after `FALLBACK_TRIALS` attempts. Hits off the fallback are also
kept out of the band contest, since the rendering emits junk tokens as well as
recovered text: a clean read on any band outranks a fallback read on every band.
See `_adaptive_crops` for the opacity sweep and the radii it chose.
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

The same floor applies to the adaptive fallback, which is why that fallback
renders the band at two local-mean radii rather than one: a single rendering
gives this interpreter nothing to compare and it would reject every recovery.
Measured on the frames in `_adaptive_crops`, the two radii agreed 0.75 to 1.00
about a recovered headline and 0.00 on six background bands of the same frame.

A news frame carries several overlays at once, so each role gets its own reader
over its own bands (the clock badge is `clock.py`, which has much stronger
evidence in a parseable time next to a named zone). Frame extraction is shared,
so a second role costs OCR on a second band, not a second decode.

Serving a news archive needs no roster, because nothing here resolves one:

```bash
dirstral-annotate serve --news --ocr-lang rus --port 8765
```

`--roster` stays required for `--scorebug`, `--jersey`, `--faces` and
play-by-play, which all resolve names against it. Point dir2mcp at the backend
as usual and each headline and ticker passage lands as a `recognition` chunk
with a `time` span, which `ask`/`search` cite directly.

#### The readability gate

Agreement decides which band holds an overlay. It does not decide whether the
words that came off that band are words. A cue therefore passes a second gate,
and this one is **on by default**: a cue is emitted only if its text is at
least 20 characters long AND it was read with agreement of at least 0.6.

Measured on 145 cues (94 headline, 51 ticker) from 15 minutes of a TV Rain
broadcast, read with tesseract `rus`. A cue counted as readable when it held a
word of 4 or more Cyrillic letters that the programme's own subtitle track also
held, so the vocabulary came from the same broadcast and needed no external
dictionary.

| gate | keeps | precision | recall |
|---|---|---|---|
| none | 145 | 40.0% | 100.0% |
| agreement >= 0.6 | 60 | 60.0% | 62.1% |
| chars >= 20 | 87 | 65.5% | 98.3% |
| **chars >= 20 and agreement >= 0.6** | **40** | **90.0%** | **62.1%** |
| chars >= 25 and agreement >= 0.6 | 36 | 91.7% | 56.9% |

Neither signal works alone. Agreement alone is flat: a floor of 0.5 gives 48.5%
precision and a floor of 1.0 gives 48.8%, while recall falls from 81.0% to
34.5%. Length alone is the stronger single signal, because the garbage is short
(median 15 characters against 32), but it tops out near 68%. Together they take
precision from 40% to 90%.

The gate is on by default because the product of this cascade is citations. The
ungated stream is 60% noise, so a citation drawn from it is more likely to be
garbage than text, and a cited span that says nothing costs a reader more than
a passage never found. The cost is real: 38% of readable passages go with it.
An operator who wants recall over precision, or who is on footage this
measurement does not describe, turns the gate off:

```bash
dirstral-annotate serve --news --news-min-chars 0 --news-min-agreement 0
```

Both floors are also parameters of `NewsOverlayRecognizer`, and
`rejected_counts()` reports how many cues per role the gate dropped, so a role
that found an overlay and could not read it stays distinct from a role that
found none.

**Known limitation.** The gate rejects a cue that is garbage. It does not clean
a cue that is PART garbage. Ticker cues can carry a long garbage prefix ahead of
their real text, where a stable graphic in the band OCRs as a run of repeated
letters. That text is real pixels being misread rather than noise, so both
preprocessing passes agree on it and it scores 0.90 like a good read. Such a
cue is long and firmly read, so it passes the gate, which is the right answer:
the words behind the prefix are intact and citable.

Four other remedies were built and measured against real footage, and NONE is
shipped:

- A token-level cleaner dropped real content (`220` out of `около 220 тысяч`,
  and the preposition `К`) while missing the actual garbage, whose character
  statistics are perfectly ordinary.
- Changing which read of a run the cue carries. Measured across two corpora,
  no candidate dominates: against the current longest-read rule, a medoid
  selector gains 2.8 points of token precision on a ticker with a graphic in
  the band and loses 4.3 precision and 3.7 recall on one without.
- Vowel fraction. The garbage has MORE vowels than readable text (0.50 against
  0.44), so it separates the wrong way.
- Character bigram plausibility. The medians separate but the ranges overlap,
  and it degenerates on short text, which is where the garbage is.

Cyrillic fraction was also measured and is 1.00 for both populations, because
the reader is pinned to one script and returns that script whatever the pixels
were.

See #751.

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

The vision cascade is slow on long media, above all on a CPU-only host, so the
bound on one `/recognize` call scales with the media's duration:

```yaml
recognize_timeout: 10m                    # flat floor; governs short media
recognize_timeout_per_media_second: 2.0   # wall-clock seconds per second of media
```

With those defaults a 3h24m broadcast gets 6h48m. Raise the ratio when the
cascade is wider than the host can keep up with. A call that runs out of its
budget does not fail the document: the annotations and transcripts already
indexed stay searchable, the run reports the error, and the next scan retries
that file. See the README section "Recognition: how long one media file may
take".

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

From the repository root, in a venv the target owns:

```bash
make test-annotator          # installs into annotator/.venv-test, then runs pytest
```

`make check` runs the same suite, so the repository's merge-readiness gate
covers this package. To drive pytest by hand, install into a venv of your own
(a PEP 668 interpreter refuses an install into itself):

```bash
python3 -m venv .venv && . .venv/bin/activate
# [test] is the runner + jsonschema; [ocr] makes the vision recognizers' tests
# run rather than skip, which is what `make test-annotator` installs (#787).
pip install -e '.[test,ocr]'
python3 -m pytest tests/    # tested code needs no network, models, or ffmpeg
```

The recognizer/fusion/eval **code** under test is stdlib-only; only the
external test tooling (`pytest`, and `jsonschema` for the contract test) is
installed via the `test` extra. The suite includes a live HTTP round-trip
against the serve handler and a schema-agreement test validating responses
against the dirstral-spec draft wire contract (a hard failure if `jsonschema`
is missing, skipped only when the `dirstral-spec` schema isn't checked out).
The dir2mcp side is pinned by `tests/ingest/recognize_test.go`.
