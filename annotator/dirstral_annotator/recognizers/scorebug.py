"""Scorebug recognizer: OCR the broadcast overlay.

The overlay names the current batter and pitcher during every at-bat, which
makes this the workhorse signal for broadcast footage that has no structured
play-by-play feed (the normal case for an archive). Unlike the play-by-play
recognizer it needs nothing but the pixels.

Reading it well takes three things, and the naive "OCR the whole frame and
fuzzy-match every line against the roster" loop got none of them:

1. *Where.* The bug occupies about 3% of the frame. Whole-frame OCR has to
   segment a crowd, a scoreboard and a sponsor board before it reaches the
   30-pixel-tall name, and usually never does. We OCR a handful of candidate
   overlay bands instead, then lock onto whichever one is actually producing
   reads so the rest of the run pays for one crop.
2. *How.* Overlay text is light-on-dark, small and JPEG-soft. Upscaling the
   crop to a fixed text height and binarising it turns a garbled read into a
   clean one; the two passes (grey and binarised) disagree often enough on
   real footage to be worth running both.
3. *What.* The bug is structured, not prose: `5.LILE` is the batter in the
   fifth lineup slot, `RAY  P: 90` is the pitcher and his pitch count,
   `SLIDER 88 MPH` is the pitch that was just thrown. Parsing those forms and
   matching only the name part against the roster is what keeps precision up:
   feeding whole OCR lines to a fuzzy matcher turns "ee" into Jung Hoo Lee and
   "#0" into the player wearing 0.

Note on the leading digit: it is the *lineup slot*, not the jersey number
(Daylen Lile bats fifth wearing 4), so it is used as a structural marker for
"this is the batter field" and never as a number-vs-roster constraint.

Consecutive frames resolving to the same player collapse into one cue
spanning the run: the overlay persisting for 20 seconds is one at-bat
sighting, not 20 observations (which would also defeat fusion's per-source
confidence cap).

The OCR engine is injected as a callable `(image_path) -> str`; the default
adapter uses pytesseract when installed.
"""

from __future__ import annotations

import difflib
import re
import tempfile
import unicodedata
from collections.abc import Callable, Iterable, Iterator
from dataclasses import dataclass
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, iter_frames

OcrFn = Callable[[Path], str]
Region = tuple[float, float, float, float]

# Fractional (x, y, w, h) boxes searched for the overlay, covering the corners
# broadcasts actually use: MLB/NBC hangs its bug top left, most other sports
# put it along the bottom. Deliberately generous, because a band that clips the
# name is worse than one that includes some crowd.
CANDIDATE_REGIONS: tuple[Region, ...] = (
    (0.00, 0.02, 0.55, 0.16),  # upper left
    (0.00, 0.80, 0.55, 0.18),  # lower left
    (0.20, 0.80, 0.60, 0.18),  # lower centre
    (0.20, 0.02, 0.60, 0.16),  # upper centre
)

# A run of frames may drop a detection for a beat (OCR flicker, or a replay
# that hides the bug); allow this many seconds of gap before closing the run.
RUN_GAP_S = 3.0

# Crops are upscaled so the band is about this tall before OCR. Fixed pixels,
# not a fixed factor, so an SD archive gets the magnification it needs and a
# 4K master is not blown up into a slow no-op.
TARGET_BAND_PX = 320
MAX_UPSCALE = 4.0
# Everything darker than this becomes black in the second pass. Overlay text is
# near-white by design, so the cut can sit high.
BINARY_THRESHOLD = 140
# tesseract page-segmentation mode for a cropped overlay strip.
OCR_PSM = 6

# Region search: while unlocked, only every Nth frame pays for the full sweep;
# once one band has produced this many reads it is the only one OCR'd.
SCAN_STRIDE = 4
LOCK_AFTER_READS = 5

# Name matching. Short reads (three or four characters: RAY, COX) are accepted
# only on an exact roster match, because that is the length at which fuzzy
# matching starts turning OCR debris into confident nonsense ("ee" scored 0.8
# against "Lee", "inn" 0.86 against "Winn").
FUZZY_MIN_LEN = 5
FUZZY_THRESHOLD = 0.85
EXACT_CONFIDENCE = 0.95
FUZZY_CEILING = 0.9
# A bare name with no structural marker around it could be anyone the graphics
# package chose to mention, so discount it against a parsed batter or pitcher
# field.
UNKNOWN_ROLE_FACTOR = 0.85
# A name off a line with no overlay structure at all is admitted only when the
# same player was read properly nearby, and never at full strength. This buys
# back the frames where OCR recovers the name but loses the row around it,
# without admitting the surnames that crowd texture invents.
LOOSE_ROLE_FACTOR = 0.7
CORROBORATION_S = 30.0

# How far a pitch-speed graphic may look for the pitcher it belongs to. The
# name field is covered by the graphic while it is up, so the pitcher has to
# come from the surrounding frames or not at all.
PITCHER_MEMORY_S = 90.0
PITCH_CUE_PAD_S = 2.0
PITCH_CONFIDENCE = 0.8
# Readings this close together are the same graphic, not two pitches: no
# pitcher works quicker than this, and the graphic itself lingers.
PITCH_MERGE_S = 6.0

BATTER, PITCHER, UNKNOWN, LOOSE = "batter", "pitcher", "unknown", "loose"

# "5.LILE", "6YOUNG": a single lineup digit punctuated onto (or glued to) the
# surname, with nothing between. The tight form is deliberate. The batter's
# day line sits between the two names, so "5.LILE 1-2. RAY P:87" offers "2. RAY"
# as a lineup field too, and a looser pattern reads the pitcher as the batter.
# The lookbehind rejects the right half of any hyphenated pair (a day line, a
# ball-strike count) for the same reason.
_LINEUP_RE = re.compile(r"(?<![0-9A-Z\-])([1-9])[.,:;]?([A-Z][A-Z'\-]{2,})")
# "GRIFFIN  P: 50": the pitch count that trails the pitcher's name.
_PITCH_COUNT_RE = re.compile(r"([A-Z][A-Z'\-]{2,})[^A-Z0-9]{0,6}P\s*[.,:;]\s*[0-9]{1,3}")
# "SLIDER 88 MPH", "FOUR SEAM 92 MPH": anchored on the literal speed unit,
# which OCR noise does not invent.
_VELOCITY_RE = re.compile(r"([A-Z][A-Z ]{2,20}?)\s*([0-9]{2,3})\s*(MPH|KPH|KM/H)")
# The unit is set in small caps and is the first thing tesseract loses (MPH
# comes back as "upeq", "mpq", "upv"), so a second anchor takes the pitch type
# instead: a known delivery followed by a plausible speed. The vocabulary is
# baseball's, like the rest of this pilot's event names.
_TYPED_SPEED_RE = re.compile(r"([A-Z][A-Z ]{3,20}?)\s*([0-9]{2,3})(?![0-9])")
PITCH_TYPES = (
    "FOUR SEAM", "FOURSEAM", "TWO SEAM", "TWOSEAM", "FASTBALL", "SINKER",
    "SLIDER", "SWEEPER", "CURVEBALL", "CURVE", "CHANGEUP", "CUTTER",
    "SPLITTER", "KNUCKLEBALL", "KNUCKLE CURVE", "SCREWBALL", "SLURVE",
)
PITCH_TYPE_THRESHOLD = 0.86
PLAUSIBLE_SPEED = range(40, 121)
_WORD_RE = re.compile(r"[A-Z][A-Z'\-]{2,}")


def default_ocr(psm: int | None = None) -> OcrFn:
    """pytesseract adapter, optionally pinned to a page-segmentation mode.

    `psm` is left alone by default because the other recognizers share this
    adapter on crops of their own shape. The scorebug asks for mode 6, "a
    single uniform block of text": tesseract's default keeps hunting for page
    structure that a 200-pixel-tall overlay strip does not have.
    """
    try:
        import pytesseract  # type: ignore
        from PIL import Image  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "scorebug OCR needs pytesseract + Pillow (pip install "
            "'dirstral-annotator[ocr]') and a tesseract binary"
        ) from exc

    config = f"--psm {psm}" if psm is not None else ""

    def ocr(frame: Path) -> str:
        with Image.open(frame) as img:
            img.load()
            return pytesseract.image_to_string(img, config=config)

    return ocr


@dataclass(frozen=True)
class NameRead:
    """One name the overlay parser recovered, with the role its position in
    the bug implies."""

    name: str
    role: str


@dataclass(frozen=True)
class PitchRead:
    """A pitch-speed graphic. The speed is always read; the type and the unit
    are whatever survived the OCR."""

    pitch_type: str
    speed: int
    unit: str = ""

    def describe(self) -> str:
        parts = [p for p in (self.pitch_type, str(self.speed), self.unit) if p]
        return " ".join(parts)


def parse_overlay(text: str) -> tuple[list[NameRead], list[PitchRead]]:
    """Pull structured fields out of one OCR pass over the bug.

    Returns the names it recognised as fields (batter, pitcher, or an
    unattributed word on a line that is demonstrably the bug) and any
    pitch-speed graphic. Nothing here touches the roster: this is purely
    "what does the overlay say".

    A bare word is only trusted (role `unknown`) when its line also yielded a
    lineup slot, a pitch count or a pitch graphic. During a replay or a crowd
    shot the bug is gone and the crop is texture, and tesseract turns texture
    into pages of three-letter words, one of which is eventually somebody's
    surname: that is where "LEE" and "LORD" came from. Structure on the line is
    the evidence that there is a bug to read at all. Words off an unstructured
    line come back as role `loose`, for the caller to corroborate or discard.
    """
    names: list[NameRead] = []
    pitches: list[PitchRead] = []
    seen: set[tuple[str, str]] = set()

    def add(name: str, role: str) -> None:
        key = (name, role)
        if key not in seen:
            seen.add(key)
            names.append(NameRead(name=name, role=role))

    for raw in text.splitlines():
        line = _fold(raw)
        if not line:
            continue
        claimed: set[str] = set()
        structured = False
        for match in _VELOCITY_RE.finditer(line):
            # The type sits directly left of the speed; anything further left
            # belongs to another field, so keep at most the last two words.
            kind = " ".join(match.group(1).split()[-2:])
            kind = kind if _is_pitch_type(kind) else ""
            speed = int(match.group(2))
            # Same plausibility gate as the typed branch: a garbled line ending
            # in digits followed by a unit is otherwise accepted at any value.
            if speed not in PLAUSIBLE_SPEED:
                continue
            pitches.append(
                PitchRead(pitch_type=kind, speed=speed, unit=match.group(3))
            )
            claimed.update(kind.split())
            claimed.add(match.group(3))
            structured = True
        for match in _TYPED_SPEED_RE.finditer(line):
            kind = " ".join(match.group(1).split()[-2:])
            speed = int(match.group(2))
            if speed in PLAUSIBLE_SPEED and _is_pitch_type(kind):
                pitches.append(PitchRead(pitch_type=kind, speed=speed))
                claimed.update(kind.split())
                structured = True
        for match in _PITCH_COUNT_RE.finditer(line):
            add(match.group(1), PITCHER)
            claimed.add(match.group(1))
            structured = True
        for match in _LINEUP_RE.finditer(line):
            add(match.group(2), BATTER)
            claimed.add(match.group(2))
            structured = True
        for word in _WORD_RE.findall(line):
            if word not in claimed:
                add(word, UNKNOWN if structured else LOOSE)
    return names, _dedupe_pitches(pitches)


class ScorebugRecognizer:
    name = "scorebug"

    def __init__(
        self,
        roster: Roster,
        ocr: OcrFn | None = None,
        fps: float = 0.5,
        crop: Region | None = None,  # fraction box; None searches for the bug
        regions: Iterable[Region] | None = None,
        pitch_cues: bool = True,
    ):
        self.roster = roster
        self.ocr = ocr if ocr is not None else default_ocr(psm=OCR_PSM)
        self.fps = fps
        self.crop = crop
        if crop is not None:
            self.regions: tuple[Region, ...] = (tuple(crop),)  # type: ignore[assignment]
        else:
            self.regions = tuple(regions) if regions is not None else CANDIDATE_REGIONS
        self.pitch_cues = pitch_cues
        self._index = _name_index(roster)

    def recognize(self, media_path: Path) -> list[Cue]:
        batters: list[tuple[float, str, float]] = []
        appearances: list[tuple[float, str, float]] = []
        pitchers: list[tuple[float, str, float]] = []
        loose: list[tuple[float, str, float]] = []
        graphics: list[tuple[float, PitchRead]] = []

        search = _RegionSearch(self.regions)
        with tempfile.TemporaryDirectory(prefix="dirstral-scorebug-") as tmp:
            work = Path(tmp)
            for i, (t, frame) in enumerate(iter_frames(media_path, fps=self.fps)):
                for region in search.regions_for(i):
                    names, pitches = self._read(frame, region, work)
                    hits = 0
                    for read in names:
                        resolved = self._resolve(read)
                        if resolved is None:
                            continue
                        pid, conf = resolved
                        if read.role == LOOSE:
                            # Not evidence of anything on its own, and not
                            # evidence that this band holds the bug either, so
                            # it does not count towards the region lock.
                            loose.append((t, pid, conf))
                            continue
                        hits += 1
                        if read.role == BATTER:
                            batters.append((t, pid, conf))
                            continue
                        if read.role == PITCHER:
                            pitchers.append((t, pid, conf))
                        # Named, but not at the plate: the pitcher between
                        # deliveries, or whoever else the graphics package
                        # chose to put on screen.
                        appearances.append((t, pid, conf))
                    hits += len(pitches)
                    graphics += [(t, pitch) for pitch in pitches]
                    search.record(region, hits)
                    if hits:
                        break  # this band answered; the others need not be OCR'd

        gap = 1.0 / self.fps
        confirmed = batters + appearances
        appearances += [s for s in loose if _corroborated(s, confirmed)]
        cues = collapse_sightings(batters, source=self.name, event="at_bat", frame_gap=gap)
        cues += collapse_sightings(
            appearances, source=self.name, event="appearance", frame_gap=gap
        )
        if self.pitch_cues:
            cues += self._pitch_cues(graphics, pitchers)
        cues.sort(key=lambda c: (c.start_s, c.event, c.entity_ids))
        return cues

    def _read(
        self, frame: Path, region: Region, work: Path
    ) -> tuple[list[NameRead], list[PitchRead]]:
        names: list[NameRead] = []
        pitches: list[PitchRead] = []
        seen: set[tuple[str, str]] = set()
        for path in _prepared_crops(frame, region, work):
            found_names, found_pitches = parse_overlay(self.ocr(path))
            for read in found_names:
                if (read.name, read.role) not in seen:
                    seen.add((read.name, read.role))
                    names.append(read)
            pitches += found_pitches
        return names, _dedupe_pitches(pitches)

    def _resolve(self, read: NameRead) -> tuple[str, float] | None:
        hit = match_name(self._index, read.name)
        if hit is None:
            return None
        pid, conf = hit
        if read.role == UNKNOWN:
            conf *= UNKNOWN_ROLE_FACTOR
        elif read.role == LOOSE:
            conf *= LOOSE_ROLE_FACTOR
        return pid, round(conf, 4)

    def _pitch_cues(
        self,
        graphics: list[tuple[float, PitchRead]],
        pitchers: list[tuple[float, str, float]],
    ) -> list[Cue]:
        """One cue per pitch-speed graphic, credited to the pitcher the bug was
        naming around it.

        This is the only per-pitch timing available without a feed, and it is
        emitted as `pitch` (the event the pilot metric scores) rather than
        folded into the pitcher's appearance run: one cue spanning a whole
        stint would claim a single long pitch instead of forty short ones.

        The graphic stays up for a few seconds, so consecutive frames see the
        same pitch (sometimes disagreeing about the speed by a digit). Readings
        close together are therefore one pitch, described by the fullest read.
        """
        cues: list[Cue] = []
        for cluster in _cluster_graphics(graphics):
            first, last = cluster[0][0], cluster[-1][0]
            pid = _nearest(pitchers, first, PITCHER_MEMORY_S)
            if pid is None:
                continue  # no idea who threw it; say nothing rather than guess
            best = max((p for _, p in cluster), key=_detail)
            cues.append(
                Cue(
                    source=self.name,
                    start_s=max(0.0, first - PITCH_CUE_PAD_S),
                    end_s=last + PITCH_CUE_PAD_S,
                    event="pitch",
                    entity_ids=(pid,),
                    confidence=PITCH_CONFIDENCE,
                    text=best.describe(),
                )
            )
        return cues


class _RegionSearch:
    """Try every candidate band until one proves itself, then stay on it.

    Sweeping four bands on every frame would cost more OCR than the whole-frame
    pass this replaces, so unlocked sweeps are strided and the winner is locked
    in after a few reads. If the overlay moves (a graphics package change part
    way through a file) the lock releases once the locked band goes quiet.
    """

    RELEASE_AFTER_MISSES = 60

    def __init__(self, regions: tuple[Region, ...]):
        self.regions = regions
        self.locked: Region | None = regions[0] if len(regions) == 1 else None
        self.scores: dict[Region, int] = {}
        self.misses = 0

    def regions_for(self, frame_index: int) -> tuple[Region, ...]:
        if self.locked is not None:
            return (self.locked,)
        if frame_index % SCAN_STRIDE:
            return ()
        return self.regions

    def record(self, region: Region, hits: int) -> None:
        if hits:
            self.scores[region] = self.scores.get(region, 0) + hits
            self.misses = 0
            if self.locked is None and self.scores[region] >= LOCK_AFTER_READS:
                self.locked = region
        elif self.locked == region and len(self.regions) > 1:
            self.misses += 1
            if self.misses >= self.RELEASE_AFTER_MISSES:
                self.locked = None
                self.scores.clear()
                self.misses = 0


def _prepared_crops(frame: Path, region: Region, work: Path) -> Iterator[Path]:
    """Yield OCR-ready renderings of one band: upscaled greyscale, and the same
    crop hard-thresholded.

    Overlay text is light on dark, which Otsu handles badly when the crop also
    catches a bright crowd; a fixed threshold recovers the name in exactly the
    frames the grey pass garbles. Both are written to the recognizer's own temp
    dir rather than beside the frame, because the frame directory is a cache
    other recognizers iterate.
    """
    try:
        from PIL import Image, ImageOps  # required by the OCR extra
    except ImportError as exc:  # pragma: no cover - exercised via the contract test
        # recognize() must degrade the cascade, not abort it, so a missing
        # Pillow has to arrive as RecognizerUnavailable rather than ImportError.
        raise RecognizerUnavailable("Pillow is required for scorebug OCR") from exc

    # load() inside the context manager pulls the pixels into memory and lets
    # the descriptor close immediately. This runs at least once per frame, so
    # relying on garbage collection to release it leaks handles across a long
    # media file and can exhaust the process limit.
    with Image.open(frame) as opened:
        opened.load()
        img = opened.copy()
    x, y, w, h = region
    box = (
        max(0, int(x * img.width)),
        max(0, int(y * img.height)),
        min(img.width, int((x + w) * img.width)),
        min(img.height, int((y + h) * img.height)),
    )
    if box[2] <= box[0] or box[3] <= box[1]:
        return
    crop = img.crop(box).convert("L")
    scale = min(MAX_UPSCALE, max(1.0, TARGET_BAND_PX / max(1, crop.height)))
    if scale > 1.0:
        crop = crop.resize(
            (int(crop.width * scale), int(crop.height * scale)), Image.LANCZOS
        )
    crop = ImageOps.autocontrast(crop)

    grey = work / "band-grey.png"
    crop.save(grey)
    yield grey

    binary = work / "band-bin.png"
    crop.point(lambda p: 255 if p > BINARY_THRESHOLD else 0).save(binary)
    yield binary


def _cluster_graphics(
    graphics: list[tuple[float, PitchRead]]
) -> list[list[tuple[float, PitchRead]]]:
    """Group pitch-speed readings that are the same graphic seen twice."""
    clusters: list[list[tuple[float, PitchRead]]] = []
    for t, pitch in sorted(graphics, key=lambda g: g[0]):
        if clusters and t - clusters[-1][-1][0] <= PITCH_MERGE_S:
            clusters[-1].append((t, pitch))
        else:
            clusters.append([(t, pitch)])
    return clusters


def _is_pitch_type(word: str) -> bool:
    """Is this OCR word one of baseball's deliveries, give or take a letter?"""
    if not word:
        return False
    return any(
        difflib.SequenceMatcher(None, word, kind).ratio() >= PITCH_TYPE_THRESHOLD
        for kind in PITCH_TYPES
    )


def _dedupe_pitches(pitches: list[PitchRead]) -> list[PitchRead]:
    """Both preprocessing passes and both speed patterns usually see the same
    graphic; keep the fullest reading of each speed."""
    best: dict[int, PitchRead] = {}
    for pitch in pitches:
        current = best.get(pitch.speed)
        if current is None or _detail(pitch) > _detail(current):
            best[pitch.speed] = pitch
    return [best[speed] for speed in sorted(best)]


def _detail(pitch: PitchRead) -> int:
    return bool(pitch.pitch_type) + bool(pitch.unit)


def _name_index(roster: Roster) -> dict[str, set[str]]:
    """Normalised name form -> player ids.

    Every roster spelling plus its surname, minus the `#number` forms
    `Player.all_names()` adds: matching those against OCR text is how a stray
    "#0" becomes a confident sighting of whoever wears 0.
    """
    index: dict[str, set[str]] = {}
    for player in roster.players:
        for form in (player.name, *player.aliases):
            key = _upper(form)
            parts = key.split()
            if not parts or len(key.replace(" ", "")) < 3:
                continue
            index.setdefault(key, set()).add(player.id)
            if len(parts[-1]) >= 3:
                index.setdefault(parts[-1], set()).add(player.id)
    return index


def match_name(index: dict[str, set[str]], token: str) -> tuple[str, float] | None:
    """Resolve one OCR name to a player id, or None.

    Exact wins. Fuzzy is allowed only for tokens long enough that a near miss
    means something, and a surname two players share resolves to neither: the
    overlay gives no way to tell them apart, so a guess would be wrong half the
    time.
    """
    key = _upper(token)
    if len(key.replace(" ", "")) < 3:
        return None
    ids = index.get(key)
    if ids:
        return (next(iter(ids)), EXACT_CONFIDENCE) if len(ids) == 1 else None
    if len(key) < FUZZY_MIN_LEN:
        return None
    best: tuple[str, float] | None = None
    for form, form_ids in index.items():
        if len(form_ids) != 1:
            continue
        ratio = difflib.SequenceMatcher(None, key, form).ratio()
        if ratio >= FUZZY_THRESHOLD and (best is None or ratio > best[1]):
            best = (next(iter(form_ids)), ratio)
    if best is None:
        return None
    return best[0], round(best[1] * FUZZY_CEILING, 4)


def _corroborated(
    loose: tuple[float, str, float], confirmed: list[tuple[float, str, float]]
) -> bool:
    """Was this player also read off a structured line close enough in time?

    "SEYMOUR" on its own is worth something when the bug named Seymour twenty
    seconds ago and nothing else has happened; the same word out of a crowd
    shot in an inning he never appears in is worth nothing.
    """
    t, pid, _ = loose
    return any(
        other_pid == pid and abs(other_t - t) <= CORROBORATION_S
        for other_t, other_pid, _ in confirmed
    )


def _nearest(
    sightings: list[tuple[float, str, float]], t: float, window: float
) -> str | None:
    best: tuple[float, str] | None = None
    for seen_t, pid, _ in sightings:
        delta = abs(seen_t - t)
        if delta <= window and (best is None or delta < best[0]):
            best = (delta, pid)
    return best[1] if best else None


def _fold(text: str) -> str:
    """Uppercase and strip accents, keeping digits and punctuation.

    The overlay writes NUNEZ, the roster writes Nuñez, and tesseract writes
    whichever it feels like; folding both to ASCII makes them one string. The
    structure (the lineup digit, the `P:` marker) has to survive, so this is
    the form the field patterns run against.
    """
    folded = unicodedata.normalize("NFKD", text)
    folded = "".join(c for c in folded if not unicodedata.combining(c))
    return folded.upper()


def _upper(text: str) -> str:
    """Fold to letters and spaces only: the form roster names are keyed by."""
    kept = [c if (c.isalpha() and c.isascii()) or c in " '-" else " " for c in _fold(text)]
    return " ".join("".join(kept).split())


def collapse_sightings(
    sightings: list[tuple[float, str, float]],
    source: str,
    event: str,
    frame_gap: float,
) -> list[Cue]:
    """Collapse per-frame (t, player_id, confidence) hits into per-run cues.

    Shared by every frame-sampling recognizer (scorebug, jersey, face).
    A run of consecutive sightings of one player becomes one cue spanning
    first..last sighting (extended by one frame interval so a single-frame
    sighting still has nonzero duration); confidence is the run's max.
    """
    runs: dict[str, list[tuple[float, float, float, float]]] = {}
    for t, pid, conf in sorted(sightings):
        pruns = runs.setdefault(pid, [])
        if pruns and t - pruns[-1][1] <= frame_gap + RUN_GAP_S:
            s, _, c, first = pruns[-1]
            pruns[-1] = (s, t, max(c, conf), first)
        else:
            pruns.append((t, t, conf, t))

    cues = []
    for pid, pruns in runs.items():
        for start, end, conf, _ in pruns:
            cues.append(
                Cue(
                    source=source,
                    start_s=start,
                    end_s=end + frame_gap,
                    event=event,
                    entity_ids=(pid,),
                    confidence=round(conf, 4),
                )
            )
    cues.sort(key=lambda c: (c.start_s, c.entity_ids))
    return cues
