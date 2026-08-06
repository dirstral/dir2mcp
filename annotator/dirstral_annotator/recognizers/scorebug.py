"""Scorebug recognizer: read a baseball broadcast's overlay against a roster.

One consumer of the generic overlay-text reader in `overlay.py`. Everything
about *finding* burned-in text and getting a clean string out of it lives
there; everything here is baseball. The split is where it is because the two
halves generalise differently: the reader reaches 96.5% identity coverage on
pilot footage with no data feed and transfers unchanged to a corpus in another
language, while the interpretation below transfers to nothing that is not a
baseball broadcast.

The overlay names the current batter and pitcher during every at-bat, which
makes this the workhorse signal for footage that has no structured play-by-play
feed (the normal case for an archive). What the parser has to get right is that
the bug is *structured*, not prose: `5.LILE` is the batter in the fifth lineup
slot, `RAY  P: 90` is the pitcher and his pitch count, `SLIDER 88 MPH` is the
pitch that was just thrown. Parsing those forms and matching only the name part
against the roster is what keeps precision up (0.541 to 0.991 on the 450-frame
fixture): feeding whole OCR lines to a fuzzy matcher turns "ee" into Jung Hoo
Lee and "#0" into the player wearing 0.

Note on the leading digit: it is the *lineup slot*, not the jersey number
(Daylen Lile bats fifth wearing 4), so it is used as a structural marker for
"this is the batter field" and never as a number-vs-roster constraint.

Consecutive frames resolving to the same player collapse into one cue spanning
the run: the overlay persisting for 20 seconds is one at-bat sighting, not 20
observations (which would also defeat fusion's per-source confidence cap).

This module also supplies the reader's band search with its evidence.
`_interpret` returns a hit count that is deliberately not "the OCR returned
something": a crop of crowd texture returns plenty. It is "the OCR returned
something this roster recognises as a field", which is the signal that made the
region search land on the bug instead of on the stands.
"""

from __future__ import annotations

import difflib
import re
import unicodedata
from collections.abc import Iterable
from contextlib import closing
from dataclasses import dataclass
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import collapse_sightings
from .overlay import (
    OCR_PSM,
    OcrFn,
    OverlayRead,
    OverlayReader,
    Region,
    default_ocr,
    default_workers,
)

# `collapse_sightings`, `OcrFn` and `default_ocr` have always been imported
# from this module by the other recognizers and by tests. Their homes are now
# `base` (cue shaping) and `overlay` (reading), but the names stay valid here.
__all__ = [
    "BATTER",
    "LOOSE",
    "PITCHER",
    "PITCH_TYPES",
    "PLAUSIBLE_SPEED",
    "UNKNOWN",
    "NameRead",
    "OcrFn",
    "PitchRead",
    "Region",
    "ScorebugRecognizer",
    "collapse_sightings",
    "default_ocr",
    "default_workers",
    "match_name",
    "parse_bands",
    "parse_overlay",
]

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
# A count transition is weaker evidence than a speed graphic: the graphic is a
# statement that a pitch was just thrown, while the count is an inference from
# two readings of a number. Confident enough to report, not as confident as a
# graphic (0.9), and above any floor an operator is likely to set.
COUNT_PITCH_CONFIDENCE = 0.75
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
# "GRIFFIN  P: 50": the pitch count that trails the pitcher's name. The digits
# are captured, not just matched: the count increments by exactly one on every
# pitch that pitcher throws and is on the bug continuously, which makes it a
# per-pitch clock that does not depend on the speed graphic being shown. See
# `_count_pitch_cues`.
_PITCH_COUNT_RE = re.compile(r"([A-Z][A-Z'\-]{2,})[^A-Z0-9]{0,6}P\s*[.,:;]\s*([0-9]{1,3})")
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


@dataclass(frozen=True)
class PitchCountRead:
    """The pitcher's cumulative pitch count as printed on the bug (`P: 90`)."""

    name: str
    count: int


@dataclass(frozen=True)
class _Fields:
    """What one band of one frame said, once the roster has had its say.

    `names` is (player id, confidence, role) so the caller does not resolve the
    same read twice; `pitches` stays unresolved because a speed graphic names
    nobody.
    """

    names: tuple[tuple[str, float, str], ...] = ()
    pitches: tuple[PitchRead, ...] = ()
    counts: tuple[PitchCountRead, ...] = ()


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
    counts: list[PitchCountRead] = []
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
            counts.append(PitchCountRead(name=match.group(1), count=int(match.group(2))))
            structured = True
        for match in _LINEUP_RE.finditer(line):
            add(match.group(2), BATTER)
            claimed.add(match.group(2))
            structured = True
        for word in _WORD_RE.findall(line):
            if word not in claimed:
                add(word, UNKNOWN if structured else LOOSE)
    return names, _dedupe_pitches(pitches), counts


def parse_bands(
    texts: Iterable[str],
) -> tuple[list[NameRead], list[PitchRead], list[PitchCountRead]]:
    """Merge one band's preprocessing passes into one set of fields.

    The grey and thresholded passes disagree, and neither is reliably better:
    44 of 215 readable frames on the pilot fixture were readable only after
    thresholding. So both are parsed and the union kept, with each (name, role)
    counted once and each speed described by its fullest reading.
    """
    names: list[NameRead] = []
    pitches: list[PitchRead] = []
    counts: list[PitchCountRead] = []
    seen: set[tuple[str, str]] = set()
    for text in texts:
        found_names, found_pitches, found_counts = parse_overlay(text)
        for read in found_names:
            if (read.name, read.role) not in seen:
                seen.add((read.name, read.role))
                names.append(read)
        pitches += found_pitches
        counts += found_counts
    return names, _dedupe_pitches(pitches), counts


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
        workers: int | None = None,  # None: default_workers(); 1: serial
        lang: str | None = None,  # OCR language; None: the reader's default
    ):
        self.roster = roster
        self.pitch_cues = pitch_cues
        self.reader = OverlayReader(
            ocr=ocr,
            fps=fps,
            crop=crop,
            regions=regions,
            workers=workers,
            lang=lang,
            psm=OCR_PSM,
            name=self.name,
        )
        # Mirrored so the recognizer still answers for its own configuration.
        # The reader owns them; these are reads of what it settled on.
        self.ocr = self.reader.ocr
        self.fps = self.reader.fps
        self.crop = self.reader.crop
        self.regions = self.reader.regions
        self.workers = self.reader.workers
        self.lang = self.reader.lang
        self._index = _name_index(roster)

    def recognize(self, media_path: Path) -> list[Cue]:
        batters: list[tuple[float, str, float]] = []
        appearances: list[tuple[float, str, float]] = []
        pitchers: list[tuple[float, str, float]] = []
        loose: list[tuple[float, str, float]] = []
        graphics: list[tuple[float, PitchRead]] = []
        counted: list[tuple[float, str, int]] = []

        # `closing` because the reader owns a worker pool and a scratch
        # directory for the length of the iteration: abandoning it part way
        # through (an error in this loop, an interrupt) has to shut them down
        # now rather than whenever the generator is collected.
        with closing(self.reader.read(media_path, self._interpret)) as reads:
            for read, fields in reads:
                t = read.timestamp_s
                for pid, conf, role in fields.names:
                    if role == LOOSE:
                        # Not evidence of anything on its own, and not
                        # evidence that this band holds the bug either, which
                        # is why `_interpret` keeps it out of the hit count.
                        loose.append((t, pid, conf))
                        continue
                    if role == BATTER:
                        batters.append((t, pid, conf))
                        continue
                    if role == PITCHER:
                        pitchers.append((t, pid, conf))
                    # Named, but not at the plate: the pitcher between
                    # deliveries, or whoever else the graphics package chose
                    # to put on screen.
                    appearances.append((t, pid, conf))
                graphics += [(t, pitch) for pitch in fields.pitches]
                for cr in fields.counts:
                    hit = match_name(self._index, cr.name)
                    if hit is not None:
                        counted.append((t, hit[0], cr.count))

        gap = 1.0 / self.fps
        confirmed = batters + appearances
        appearances += [s for s in loose if _corroborated(s, confirmed)]
        cues = collapse_sightings(batters, source=self.name, event="at_bat", frame_gap=gap)
        cues += collapse_sightings(
            appearances, source=self.name, event="appearance", frame_gap=gap
        )
        if self.pitch_cues:
            graphic_cues = self._pitch_cues(graphics, pitchers)
            cues += graphic_cues
            cues += _count_pitch_cues(counted, graphic_cues, self.name, gap)
        cues.sort(key=lambda c: (c.start_s, c.event, c.entity_ids))
        return cues

    def _interpret(self, read: OverlayRead) -> tuple[_Fields, int]:
        """Turn one band's text into roster-resolved fields plus a hit count.

        The hit count is what the reader's band search runs on, and it counts
        only reads this roster recognises off a structured line, plus pitch
        graphics. A loose word never counts even when it resolves: it is as
        likely to have come out of the stands as off the bug, and letting it
        vote would let crowd texture win the band search.
        """
        names, pitches, counts = parse_bands(read.texts)
        resolved: list[tuple[str, float, str]] = []
        hits = 0
        for name_read in names:
            hit = self._resolve(name_read)
            if hit is None:
                continue
            pid, conf = hit
            resolved.append((pid, conf, name_read.role))
            if name_read.role != LOOSE:
                hits += 1
        return _Fields(tuple(resolved), tuple(pitches), tuple(counts)), hits + len(pitches)

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

    Latin-specific, and deliberately not shared with the reader: decomposing
    and dropping combining marks is right for Nuñez and wrong for Cyrillic,
    where it would fold Russian's short i onto a plain i.
    """
    folded = unicodedata.normalize("NFKD", text)
    folded = "".join(c for c in folded if not unicodedata.combining(c))
    return folded.upper()


def _upper(text: str) -> str:
    """Fold to letters and spaces only: the form roster names are keyed by."""
    kept = [c if (c.isalpha() and c.isascii()) or c in " '-" else " " for c in _fold(text)]
    return " ".join("".join(kept).split())


#: How close a count-derived pitch may sit to a graphic-derived one before it is
#: treated as the same pitch. The metric's own tolerance, because two cues that
#: both fall within tolerance of one ground-truth pitch are two claims about one
#: event, and the second can only ever be a false positive.
COUNT_PITCH_DEDUPE_S = 5.0


def _count_pitch_cues(
    counted: list[tuple[float, str, int]],
    graphic_cues: list[Cue],
    source: str,
    gap: float,
) -> list[Cue]:
    """One `pitch` cue per observed increment of the bug's pitch count.

    The speed graphic is shown for some pitches and not others: on the pilot
    game it yielded 299 cues against 344 thrown, and every one of those 299 was
    credited, so attribution was never the problem — finding the pitch was. The
    count printed beside the pitcher's own name (`RAY  P: 90`) is on the bug
    continuously and rises by exactly one per pitch, so a transition is a pitch
    with a timestamp, already attributed to the pitcher whose field it sits in.
    It was being matched to identify the pitcher and its digits thrown away.

    Only a +1 step counts. A larger jump means frames were missed or a digit was
    misread, and while pitches certainly happened, WHEN is unknown; emitting
    them at the moment the jump was noticed would be inventing timing the bug
    never showed. A decrease is a new pitcher or OCR noise and resets the
    tracking for that pitcher, never emits.

    Cues that land on a pitch a graphic already reported are dropped rather than
    added. The scorecard matches at most one annotation per ground-truth pitch
    and counts the rest as false positives, so a duplicate cannot raise recall
    and can only cost precision, which is currently perfect.
    """
    cues: list[Cue] = []
    last: dict[str, int] = {}
    for t, pid, count in sorted(counted):
        previous, seen = last.get(pid), pid in last
        last[pid] = count
        if not seen or previous is None:
            continue  # first sighting establishes the baseline, claims nothing
        if count != previous + 1:
            continue
        cues.append(
            Cue(
                source=source,
                start_s=max(0.0, t - PITCH_CUE_PAD_S),
                end_s=t + PITCH_CUE_PAD_S,
                event="pitch",
                entity_ids=(pid,),
                confidence=COUNT_PITCH_CONFIDENCE,
                text=f"pitch {count}",
            )
        )
    return _drop_pitches_already_reported(cues, graphic_cues)


def _drop_pitches_already_reported(cues: list[Cue], reported: list[Cue]) -> list[Cue]:
    """Keep only cues that do not describe a pitch some other cue already did."""
    kept: list[Cue] = []
    for cue in cues:
        midpoint = (cue.start_s + cue.end_s) / 2.0
        duplicate = False
        for other in reported:
            if not set(cue.entity_ids).intersection(other.entity_ids):
                continue
            other_mid = (other.start_s + other.end_s) / 2.0
            if abs(midpoint - other_mid) <= COUNT_PITCH_DEDUPE_S:
                duplicate = True
                break
        if not duplicate:
            kept.append(cue)
    return kept
