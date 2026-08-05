"""Burned-in clock reader: turn a broadcast's own clock badge into a wall-clock
anchor for the file.

Every citation an archive can make is relative to a video timestamp, and a
video timestamp means nothing on its own: "at 00:42:10" is only useful once
something says what wall-clock instant 00:42:10 was. Deriving that anchor is
normally manual and expensive. For the baseball pilot it took a scorebug
transition cross-referenced against a structured feed, repeated at the other
end of the file to check for drift, and it was only possible because a feed
existed at all.

Broadcast news does not need the feed. It burns the clock into the frame, so
the anchor is written on the picture: read `21:35 MSK` at video t=300 and the
file started at 21:30 local. This module reads that badge off the generic
overlay reader in `overlay.py` and turns a run of readings into an anchor.

Four things make that harder than parsing a string, and all four are why this
is a module rather than a regex:

1. **A wrong anchor is worse than no anchor.** It is inherited by every
   citation the file ever produces, silently. OCR on a frame where the badge is
   absent or occluded returns confident nonsense, and on a frame where it is
   present it still sometimes returns the wrong digit: on the sample used to
   build this, one frame showing `21:35` read as `21:55`, which is a plausible
   time, in the right format, with the right zone label, and twenty minutes
   wrong. Nothing about that single reading is detectably bad. So no single
   reading is ever trusted: readings are grouped by the anchor they imply and a
   group has to reach `min_readings` before it is an anchor at all.

2. **The badge shows minutes, so a reading is an interval, not a point.** A
   badge reading `21:35` was displayed for the whole minute, so one reading
   only says the anchor is somewhere in a 60 second window. That makes the
   anchor an intersection of intervals rather than an average of points, which
   is both more honest and much tighter: see `_intersect`. It also inverts what
   spread means. Readings that all imply the same offset have sampled the same
   phase of the minute and pin nothing; readings that disagree by 59 seconds
   have bracketed the minute boundary and pin the anchor to a second. Spread is
   information, not error, which is the opposite of the usual reading.

3. **Long files are edited.** A recording is not always one continuous take,
   and a cut moves the anchor for everything after it. Readings from the start
   and the end of a file that do not agree are reported as separate segments
   rather than averaged into one anchor that is wrong for both halves.

4. **A zone label is data.** `MSK`, `ET`, `CET` are printed by the broadcaster
   and mean nothing to a reader that does not know them, and guessing the
   reader's own local zone is how an anchor ends up three hours out with no
   evidence of it. The zone table is a required argument; a time whose label is
   not in it is not a reading. The date is data too, and one the badge does not
   carry: a badge gives a time of day, so an epoch needs a date from somewhere
   else. Without one this returns the time-of-day anchor and declines to invent
   an epoch. (The container's `creation_time` is not that date. On the sample
   file it was the upload's timestamp, eleven hours off the badge.)

Nothing here knows a corpus, a language or a broadcaster. The zone table, the
OCR language, the candidate regions and the sampling rate are all arguments.
"""

from __future__ import annotations

import re
from collections.abc import Iterable, Iterator, Mapping
from contextlib import closing
from dataclasses import dataclass, field
from datetime import date as Date
from datetime import datetime, time, timedelta, timezone, tzinfo
from pathlib import Path

from .overlay import (
    OcrFn,
    OverlayRead,
    OverlayReader,
    Region,
)

__all__ = [
    "CLOCK_PSM",
    "CLOCK_REGIONS",
    "ClockAnchor",
    "ClockRead",
    "ClockReader",
    "ClockSegment",
    "ParsedTime",
    "parse_times",
    "resolve_zones",
]

DAY_S = 86400
MINUTE_S = 60

# Candidate bands for a clock badge: the four corners, generously sized. A
# badge is a few percent of the frame and every broadcaster puts it in a
# corner, but not the same corner. These are deliberately small, and that is
# load bearing rather than an optimisation: the same badge that reads cleanly
# out of a corner box returns nothing at all out of `overlay.WIDE_REGIONS`,
# because a wide band buries a two-word island in mostly-empty picture. The
# reader's own band search picks whichever of these actually holds a clock and
# then pays for one crop per frame.
CLOCK_REGIONS: tuple[Region, ...] = (
    (0.00, 0.00, 0.30, 0.14),  # upper left
    (0.70, 0.00, 0.30, 0.14),  # upper right
    (0.00, 0.86, 0.30, 0.14),  # lower left
    (0.70, 0.86, 0.30, 0.14),  # lower right
)

# tesseract page-segmentation mode 11, "sparse text: find as much text as
# possible in no particular order". Measured, and it is the difference between
# reading the badge and not reading it: on the sample frames psm 6 (the
# overlay reader's default, "one uniform block") returned only the fragment of
# the LIVE flag next to the badge and never the time, under either preprocessing
# pass, at every crop size tried. psm 6 is right for a dense strip like a
# scorebug and wrong for an island of text in an otherwise empty corner.
CLOCK_PSM = 11

# How far either side of a time a zone label may sit and still be taken as
# belonging to it. Wide enough for "21:35  MSK" with OCR's invented spaces,
# narrow enough that a label somewhere else in the crop does not adopt an
# unrelated number.
ZONE_WINDOW = 10

#: A group of readings has to be at least this large before it is an anchor.
#: One confident misread is a group of one; the guard against it is corroboration
#: and nothing else, because a single reading carries no evidence of its own
#: correctness.
MIN_READINGS = 3

# Hours and minutes, with the separator OCR might have mangled (a colon comes
# back as a dot or a semicolon often enough to be worth accepting), and optional
# seconds for badges that show them. The lookarounds keep a longer digit run
# from being read as a time: "12754" must not yield 12:75.
_TIME_RE = re.compile(
    r"(?<![0-9])([0-9]{1,2})\s*[:;.]\s*([0-9]{2})(?:\s*[:;.]\s*([0-9]{2}))?(?![0-9])"
)
# Everything that is not a letter or a digit is separator, so a zone label can
# be recovered from whatever punctuation OCR sprayed around it.
_NOT_WORD_RE = re.compile(r"[^\w]+", re.UNICODE)


def resolve_zones(zones: Mapping[str, str | int | tzinfo]) -> dict[str, tzinfo]:
    """Normalise a badge-label to timezone table into label -> tzinfo.

    A value may be an IANA name ("Europe/Moscow"), a fixed offset in seconds
    (10800), or a tzinfo. IANA names are resolved through `zoneinfo`, so a
    zone with a DST history is applied correctly for the anchor's own date
    rather than at today's offset; fixed offsets are for badges that name an
    offset rather than a zone, and for hosts with no tz database.

    Labels are matched case-insensitively and with punctuation stripped, so
    "MSK", "msk" and "M.S.K" are one entry.
    """
    resolved: dict[str, tzinfo] = {}
    for label, spec in zones.items():
        key = _fold_label(label)
        if not key:
            raise ValueError(f"zone label is empty after folding: {label!r}")
        resolved[key] = _as_tzinfo(spec, label)
    return resolved


def _as_tzinfo(spec: str | int | tzinfo, label: str) -> tzinfo:
    if isinstance(spec, tzinfo):
        return spec
    if isinstance(spec, int):
        return timezone(timedelta(seconds=spec))
    try:
        from zoneinfo import ZoneInfo
    except ImportError as exc:  # pragma: no cover - zoneinfo is stdlib on 3.9+
        raise ValueError(f"cannot resolve zone {spec!r} for {label!r}") from exc
    try:
        return ZoneInfo(spec)
    except Exception as exc:
        # A missing tz database is a configuration problem the caller has to
        # see, not something to paper over with a guessed offset.
        raise ValueError(
            f"unknown timezone {spec!r} for badge label {label!r}: {exc}"
        ) from exc


def _fold_label(label: str) -> str:
    return _NOT_WORD_RE.sub("", label).upper()


@dataclass(frozen=True)
class ParsedTime:
    """One time of day found in one OCR string, with the zone it was labelled
    with. Purely what the text said; nothing here has been corroborated."""

    seconds_of_day: int
    zone: str  # the folded label, as it keys the zone table

    @property
    def hour(self) -> int:
        return self.seconds_of_day // 3600

    @property
    def minute(self) -> int:
        return self.seconds_of_day // 60 % 60


def parse_times(
    text: str,
    zones: Iterable[str],
    default_zone: str | None = None,
) -> list[ParsedTime]:
    """Every plausible zone-labelled time of day in one OCR pass.

    Plausible is doing real work: OCR of a corner that holds no badge returns
    digits, and digits with a colon in them are not rare (a poll panel, a
    duration, a score). Three things have to hold before a match is a reading.
    The hour must be under 24 and the minute under 60, so `21:75` is debris.
    The digits must not be part of a longer run, so `12754` is not `12:75`. And
    a zone label the caller knows must sit next to it, which is the strongest of
    the three, because it is the one piece of the badge that a stray number
    cannot accidentally satisfy.

    `default_zone` is for a broadcaster that prints an unlabelled clock; it is
    an explicit statement by the caller about that corpus, never a fallback
    this module chooses. With no default, an unlabelled time is not a reading.
    """
    known = {_fold_label(z) for z in zones}
    default = _fold_label(default_zone) if default_zone else None
    found: list[ParsedTime] = []
    for match in _TIME_RE.finditer(text):
        hour, minute = int(match.group(1)), int(match.group(2))
        second = int(match.group(3) or 0)
        if hour > 23 or minute > 59 or second > 59:
            continue
        zone = _zone_beside(text, match.start(), match.end(), known)
        if zone is None:
            if default is None:
                continue
            zone = default
        found.append(
            ParsedTime(seconds_of_day=hour * 3600 + minute * 60 + second, zone=zone)
        )
    return found


def _zone_beside(text: str, start: int, end: int, known: set[str]) -> str | None:
    """The zone label printed immediately after (or before) a time, if any.

    Anchored to the time rather than searched for anywhere in the window: a
    label has to be the next thing printed after the digits (or the last thing
    before them) once punctuation and OCR's invented spaces are folded out.
    A loose substring test would let a two-letter label like `ET` match inside
    any word that happens to contain it.

    Anchoring alone is not enough, because folding is what removes the word
    boundaries: `11:20 ETA 5 min` folds to `ETA5MIN`, which starts with `ET`,
    and `TICKET 11:20` folds to `TICKET`, which ends with it. Both read as a
    labelled time before this check.

    So the boundary is checked against the ORIGINAL text rather than the folded
    window. Folding cannot answer the question it creates: `MSK LIVE` and
    `MSKVA` fold to strings that both continue with a letter past `MSK`, and
    the first is a real badge while the second is not. Keeping each folded
    character's source index lets the fold do what it is for (`11:20 E T` is
    what OCR returns for a letter-spaced badge) while the word boundary is
    still read off the text as printed.
    """
    after, after_at = _fold_indexed(text, end, min(len(text), end + ZONE_WINDOW))
    lo = max(0, start - ZONE_WINDOW)
    before, before_at = _fold_indexed(text, lo, start)
    for label in sorted(known, key=len, reverse=True):
        if not label:
            continue
        n = len(label)
        if after.startswith(label) and not _letter_after(text, after_at[n - 1]):
            return label
        if before.endswith(label) and not _letter_before(text, before_at[-n]):
            return label
    return None


def _fold_indexed(text: str, lo: int, hi: int) -> tuple[str, list[int]]:
    """`_fold_label` over `text[lo:hi]`, plus each kept character's source index."""
    folded: list[str] = []
    where: list[int] = []
    for offset in range(lo, hi):
        char = text[offset]
        if not _NOT_WORD_RE.match(char):
            folded.append(char.upper())
            where.append(offset)
    return "".join(folded), where


def _letter_before(text: str, index: int) -> bool:
    return index > 0 and text[index - 1].isalpha()


def _letter_after(text: str, index: int) -> bool:
    return index + 1 < len(text) and text[index + 1].isalpha()


@dataclass(frozen=True)
class ClockRead:
    """One frame's accepted badge reading."""

    timestamp_s: float  # video time this frame was sampled at
    seconds_of_day: int  # what the badge said, in its own zone
    zone: str
    #: The OCR passes it was reconciled from. Kept out of the repr: a real file
    #: yields a hundred readings of several passes each, and a bare
    #: `print(anchor)` would otherwise print every one of them.
    texts: tuple[str, ...] = field(default=(), repr=False)

    @property
    def implied_offset_s(self) -> float:
        """Wall-clock second of the day that video t=0 fell on, per this frame.

        Modulo a day, so a file that runs past midnight still yields one
        consistent number.
        """
        return (self.seconds_of_day - self.timestamp_s) % DAY_S


@dataclass(frozen=True)
class ClockSegment:
    """A run of readings that agree on one anchor, and the interval they pin it
    to.

    `lower_s` and `upper_s` bound the wall-clock second of the day that video
    t=0 fell on. They are an intersection of every reading's own 60 second
    window, so the segment is only as wide as the readings leave it, and they
    are kept unwrapped (they may sit outside [0, 86400)) so the arithmetic
    stays monotonic across midnight. `offset_s` is the midpoint, wrapped.
    """

    zone: str
    lower_s: float
    upper_s: float
    first_s: float  # video time of the first reading in the segment
    last_s: float
    #: Kept out of the generated repr for the same reason as `ClockRead.texts`;
    #: `__repr__` below reports the count instead, which is what a caller
    #: inspecting a segment actually wants to see.
    readings: tuple[ClockRead, ...] = field(default_factory=tuple, repr=False)

    def __repr__(self) -> str:
        return (
            f"ClockSegment(zone={self.zone!r}, lower_s={self.lower_s!r}, "
            f"upper_s={self.upper_s!r}, first_s={self.first_s!r}, "
            f"last_s={self.last_s!r}, readings={len(self.readings)})"
        )

    @property
    def offset_s(self) -> float:
        """Best single estimate of the wall clock at video t=0, as a second of
        the day."""
        return ((self.lower_s + self.upper_s) / 2.0) % DAY_S

    @property
    def uncertainty_s(self) -> float:
        """Half the width of the interval: the most this anchor can be out by."""
        return (self.upper_s - self.lower_s) / 2.0

    @property
    def spread_s(self) -> float:
        """How far apart the readings' implied offsets were.

        The complement of the interval width, and the reason spread is worth
        reporting: readings that disagree have sampled different phases of the
        badge's minute and have bracketed it. Zero spread means every reading
        landed on the same phase and the anchor is only known to a minute.
        """
        return MINUTE_S - (self.upper_s - self.lower_s)

    @property
    def count(self) -> int:
        return len(self.readings)


@dataclass(frozen=True)
class ClockAnchor:
    """What a file's clock badge established, and how firmly.

    `segments` is every group of readings that agreed among themselves; more
    than one means the readings do not describe a single continuous recording,
    and the extras are reported rather than averaged away. `segment` is the
    largest, and the one the epoch and the offsets come from.
    """

    segment: ClockSegment
    segments: tuple[ClockSegment, ...]
    zone_label: str
    tz: tzinfo | None = None
    anchor_date: Date | None = None
    readings_total: int = 0  # accepted frame readings, before grouping
    conflicts: int = 0  # frames whose OCR passes contradicted each other
    bands_read: int = 0  # band crops the reader offered, locked or sweeping

    @property
    def offset_s(self) -> float:
        return self.segment.offset_s

    @property
    def uncertainty_s(self) -> float:
        return self.segment.uncertainty_s

    @property
    def spread_s(self) -> float:
        return self.segment.spread_s

    @property
    def drifted(self) -> tuple[ClockSegment, ...]:
        """Segments that describe a stretch of video this anchor does not.

        A second anchor over video the first never covered is an edit: the
        recording is not one continuous take, and citations after the cut
        belong to a different wall clock. Reported, never averaged in.
        """
        return tuple(
            segment
            for segment in self.segments
            if segment is not self.segment and not _overlapping(segment, self.segment)
        )

    @property
    def contested(self) -> tuple[ClockSegment, ...]:
        """Segments claiming a different anchor for video this one also covers.

        Not an edit, and worth separating from one: no stretch of video has two
        wall clocks, so a segment whose readings sit inside the anchor's own
        span is a run of misreads that happened to agree. They do agree, often
        enough to pass any corroboration count on their own; the measured case
        was fourteen consecutive frames of a badge reading 21:40 that OCR'd as
        23:30. Overlap in video time is what tells the two apart, and it costs
        nothing to check.
        """
        return tuple(
            segment
            for segment in self.segments
            if segment is not self.segment and _overlapping(segment, self.segment)
        )

    @property
    def drift(self) -> bool:
        """Does the file imply more than one anchor over more than one stretch?"""
        return bool(self.drifted)

    def segment_for(self, video_s: float) -> ClockSegment | None:
        """The segment whose readings cover this video timestamp, if any.

        For a file whose anchor moves part way through, this is how a consumer
        cites the second half correctly rather than through the anchor derived
        from the first. Outside every segment's span it returns None: nothing
        was read there, and extrapolating across a cut is what produced the
        wrong answer in the first place.
        """
        covering = [
            segment
            for segment in self.segments
            if segment.first_s <= video_s <= segment.last_s
        ]
        if not covering:
            return None
        return max(covering, key=lambda segment: segment.count)

    @property
    def agreement(self) -> float:
        """Share of accepted readings that back the reported anchor."""
        return self.segment.count / self.readings_total if self.readings_total else 0.0

    @property
    def epoch_s(self) -> float | None:
        """Unix epoch of video t=0, or None when no date was supplied.

        The badge names a time of day and no date, so this is None unless the
        caller said which day the file starts on. Returning a guess would be
        the one failure this module exists to prevent.
        """
        if self.tz is None or self.anchor_date is None:
            return None
        return self._at(self.anchor_date, self.offset_s).timestamp()

    def wall_clock_at(self, video_s: float) -> datetime | None:
        """Aware datetime of a video timestamp, or None without a date.

        Aware, always: an anchor that a caller has to interpret in its own
        local zone is an anchor that will eventually be interpreted in the
        wrong one.
        """
        epoch = self.epoch_s
        if epoch is None or self.tz is None:
            return None
        return datetime.fromtimestamp(epoch + video_s, tz=self.tz)

    def time_of_day_at(self, video_s: float) -> float:
        """Second of the day a video timestamp falls on, with no date needed."""
        return (self.offset_s + video_s) % DAY_S

    def _at(self, day: Date, seconds: float) -> datetime:
        assert self.tz is not None
        midnight = datetime.combine(day, time(0, 0), tzinfo=self.tz)
        # Rebuilt through the zone rather than added to a UTC instant, so a
        # local midnight and a DST change on the same day stay consistent.
        return (midnight + timedelta(seconds=seconds)).astimezone(self.tz)

    def describe(self) -> str:
        """One line, for a log or an eval report."""
        notes = [
            f"{self.segment.count}/{self.readings_total} readings",
            f"spread {self.spread_s:.1f}s",
        ]
        if self.drifted:
            notes.append(f"DRIFT: {len(self.drifted)} other stretch(es) of this file")
        if self.contested:
            notes.append(f"{len(self.contested)} contested")
        return (
            f"t=0 is {_hhmmss(self.offset_s)} {self.zone_label} "
            f"+/-{self.uncertainty_s:.1f}s (" + ", ".join(notes) + ")"
        )


def _overlapping(one: ClockSegment, other: ClockSegment) -> bool:
    """Do two segments' readings cover any of the same video?"""
    return one.first_s <= other.last_s and other.first_s <= one.last_s


class ClockReader:
    """Read a burned-in clock badge and derive the file's wall-clock anchor.

    One consumer of the generic overlay reader, in the same shape as the
    scorebug recognizer: everything about finding burned-in text lives in
    `overlay.py`, and everything about what a clock badge is lives here. The
    interpretation it hands back to the reader counts a band as a hit only when
    a plausible zone-labelled time came out of it, which is what keeps the band
    search off the crowd, the ticker and the poll panel: all of them return
    text, none of them return a time next to a zone the caller named.

    `zones` maps badge labels to timezones and is required. It is how the caller
    says what the corpus prints, and there is no default: a badge label this
    reader does not know is a badge it declines to read, which is the correct
    outcome for a module whose worst failure mode is a confident wrong answer.
    """

    name = "clock"

    def __init__(
        self,
        zones: Mapping[str, str | int | tzinfo],
        ocr: OcrFn | None = None,
        fps: float = 0.5,
        crop: Region | None = None,  # fraction box; None searches the corners
        regions: Iterable[Region] | None = None,
        workers: int | None = None,  # None: the reader's default; 1: serial
        lang: str | None = None,  # OCR language; None: the reader's default
        psm: int | None = CLOCK_PSM,
        default_zone: str | None = None,  # for badges that print no label
        min_readings: int = MIN_READINGS,
        anchor_date: Date | None = None,  # calendar date of video t=0
    ):
        self.zones = resolve_zones(zones)
        if default_zone is not None and _fold_label(default_zone) not in self.zones:
            raise ValueError(f"default_zone {default_zone!r} is not in the zone table")
        self.default_zone = default_zone
        self.min_readings = max(1, int(min_readings))
        self.anchor_date = anchor_date
        self.reader = OverlayReader(
            ocr=ocr,
            fps=fps,
            crop=crop,
            regions=CLOCK_REGIONS if (crop is None and regions is None) else regions,
            workers=workers,
            lang=lang,
            psm=psm,
            name=self.name,
        )
        # Mirrored so the reader answers for its own configuration, as the
        # other recognizers do. The overlay reader owns them.
        self.fps = self.reader.fps
        self.regions = self.reader.regions
        self.workers = self.reader.workers
        self.lang = self.reader.lang
        self._conflicts = 0
        self._bands = 0

    def read_clock(self, media_path: Path) -> list[ClockRead]:
        """Every frame reading the badge produced, in video order."""
        self._conflicts = 0
        self._bands = 0
        reads: list[ClockRead] = []
        # `closing` because the overlay reader holds a worker pool and a scratch
        # directory for the length of the iteration: leaving this loop early has
        # to shut them down now, not whenever the generator is collected.
        with closing(self.reader.read(media_path, self._interpret)) as stream:
            for _, found in stream:
                if found is not None:
                    reads.append(found)
        return reads

    def anchor(self, media_path: Path) -> ClockAnchor | None:
        """Derive the file's wall-clock anchor, or None if it cannot be.

        None is a real answer and the common one: most footage carries no clock
        badge, and this returns nothing at all for it rather than an anchor
        assembled out of whatever digits the corners happened to hold.
        """
        return self.anchor_from(self.read_clock(media_path))

    def anchor_from(self, reads: Iterable[ClockRead]) -> ClockAnchor | None:
        """The derivation on its own, for readings gathered elsewhere.

        Separate from the reading so an anchor can be recomputed, or derived
        from readings taken out of several windows of one file, without paying
        for the OCR again. The counters it reports (`conflicts`, `bands_read`)
        describe this reader's last `read_clock` and carry over from it: they
        are zero only on an instance that has never run one. On an instance
        that has, they still describe that run and not the readings passed in
        here, so read them from the reader that took the readings.
        """
        readings = list(reads)
        segments = _segments(readings, self.min_readings)
        if not segments:
            return None
        # Largest first: the anchor most of the file's readings agree on, with
        # ties going to the earlier one so the answer does not depend on
        # dictionary order.
        ranked = sorted(segments, key=lambda s: (-s.count, s.first_s))
        best = ranked[0]
        return ClockAnchor(
            segment=best,
            segments=tuple(sorted(segments, key=lambda s: s.first_s)),
            zone_label=best.zone,
            tz=self.zones.get(best.zone),
            anchor_date=self.anchor_date,
            readings_total=len(readings),
            conflicts=self._conflicts,
            bands_read=self._bands,
        )

    def _interpret(self, read: OverlayRead) -> tuple[ClockRead | None, int]:
        """Reconcile one band's preprocessing passes into at most one reading.

        The passes disagree: on the sample frames the grey pass read the badge
        on some frames and returned nothing on others, and the thresholded pass
        did the same on a different set, which is the reason both are run. Where
        exactly one time survives across the passes it is the reading. Where two
        survive, the frame is dropped and counted as a conflict, because the
        passes contradicting each other is the only in-band evidence available
        that this frame's OCR is unreliable, and a frame is cheap while a wrong
        anchor is not.

        The hit count is 1 whenever a plausible time was seen at all, conflict
        or not: the band search asks "is the clock here", which a contradicted
        read still answers, and it must not lose the band over a frame this
        method then refuses to use.
        """
        self._bands += 1
        seen: set[tuple[int, str]] = set()
        for text in read.texts:
            for parsed in parse_times(text, self.zones, self.default_zone):
                seen.add((parsed.seconds_of_day, parsed.zone))
        if not seen:
            return None, 0
        if len(seen) > 1:
            self._conflicts += 1
            return None, 1
        seconds_of_day, zone = seen.pop()
        return (
            ClockRead(
                timestamp_s=read.timestamp_s,
                seconds_of_day=seconds_of_day,
                zone=zone,
                texts=read.texts,
            ),
            1,
        )


def _segments(reads: list[ClockRead], min_readings: int) -> list[ClockSegment]:
    """Group readings into the anchors they are consistent with.

    Each reading says the anchor lies in a 60 second window (the badge showed
    that minute for the whole of it), so a set of readings is consistent
    exactly when their windows intersect. Groups are grown from the earliest
    implied offset upwards, which is the standard sweep for covering points
    with fixed-width intervals and yields the fewest groups.

    Grouping per zone label, because two labels are two different claims about
    what the clock means even when they resolve to the same offset.
    """
    segments: list[ClockSegment] = []
    for zone in sorted({read.zone for read in reads}):
        same_zone = [read for read in reads if read.zone == zone]
        for group in _consistent_groups(same_zone):
            if len(group) < min_readings:
                continue  # one confident misread is a group of one
            lower, upper = _intersect(group)
            segments.append(
                ClockSegment(
                    zone=zone,
                    lower_s=lower,
                    upper_s=upper,
                    first_s=min(read.timestamp_s for read in group),
                    last_s=max(read.timestamp_s for read in group),
                    readings=tuple(sorted(group, key=lambda r: r.timestamp_s)),
                )
            )
    return segments


def _consistent_groups(reads: list[ClockRead]) -> Iterator[list[ClockRead]]:
    if not reads:
        return
    reference = reads[0].implied_offset_s
    ordered = sorted(reads, key=lambda r: _unwrap(r.implied_offset_s, reference))
    group: list[ClockRead] = []
    floor = 0.0
    for read in ordered:
        offset = _unwrap(read.implied_offset_s, reference)
        if group and offset - floor >= MINUTE_S:
            yield group
            group = []
        if not group:
            floor = offset
        group.append(read)
    if group:
        yield group


def _unwrap(offset: float, reference: float) -> float:
    """Put an offset on the same side of midnight as `reference`.

    Offsets are seconds of the day, so an anchor near midnight is read as both
    ~0 and ~86400 by different frames. Comparing them raw would split one
    anchor into two segments a day apart.
    """
    return reference + (offset - reference + DAY_S / 2) % DAY_S - DAY_S / 2


def _intersect(group: list[ClockRead]) -> tuple[float, float]:
    """The window every reading in the group agrees the anchor lies in.

    A badge reading M was displayed from M:00 until M:59, so the anchor is at
    or after `implied` and less than a minute after it. Intersecting those
    windows gives [max implied, min implied + 60), and this is where the
    minute-flip refinement falls out for free rather than needing a search: two
    readings four seconds apart that straddle a minute boundary are 56 seconds
    apart in implied offset, and their intersection is four seconds wide. No
    special case looks for the flip; sampling faster simply makes the interval
    tighter, down to the frame interval.
    """
    reference = group[0].implied_offset_s
    offsets = [_unwrap(read.implied_offset_s, reference) for read in group]
    return max(offsets), min(offsets) + MINUTE_S


def _hhmmss(seconds: float) -> str:
    whole = int(seconds) % DAY_S
    return f"{whole // 3600:02d}:{whole // 60 % 60:02d}:{whole % 60:02d}"
