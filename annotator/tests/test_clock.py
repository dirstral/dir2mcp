"""The burned-in clock reader: what it must derive, and what it must refuse.

A wall-clock anchor is inherited by every citation the file produces, so the
tests that matter most here are the ones about declining. A reader that returns
an anchor for footage with no clock, or that averages an OCR misread into a
real one, is worse than no reader at all: the failure is silent and it is
downstream of everything.

Backend-free. The OCR engine is a callable and the frames are scripted, so
nothing here needs tesseract, Pillow or a media file. No corpus, language or
broadcaster shows up as an assumption: the badges below are invented, and the
zone labels are the caller's data in every test that uses one.
"""

from __future__ import annotations

import ast
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import pytest

from dirstral_annotator.recognizers import clock, overlay
from dirstral_annotator.recognizers.clock import (
    ClockRead,
    ClockReader,
    parse_times,
    resolve_zones,
)

MEDIA = "broadcast.mp4"
BADGE = (0.00, 0.00, 0.30, 0.14)
NOISE = (0.70, 0.86, 0.30, 0.14)
ZONES = {"MSK": 3 * 3600}
UTC = timezone.utc


def _badge(seconds_of_day: float, zone: str = "MSK") -> str:
    """What a badge showing whole minutes prints at this second of the day."""
    whole = int(seconds_of_day) % clock.DAY_S
    return f"{whole // 3600:02d}:{whole // 60 % 60:02d} {zone}"


# --- parsing: what counts as a reading -------------------------------------

def test_a_labelled_time_parses_and_keeps_its_label():
    (parsed,) = parse_times("21:35 MSK LIVE", ZONES)
    assert (parsed.hour, parsed.minute, parsed.zone) == (21, 35, "MSK")
    assert parsed.seconds_of_day == 21 * 3600 + 35 * 60


def test_the_label_may_be_printed_before_the_time():
    (parsed,) = parse_times("MSK 09:05", ZONES)
    assert parsed.seconds_of_day == 9 * 3600 + 5 * 60


def test_seconds_are_kept_when_the_badge_shows_them():
    (parsed,) = parse_times("09:05:42 MSK", ZONES)
    assert parsed.seconds_of_day == 9 * 3600 + 5 * 60 + 42


def test_ocr_punctuation_does_not_lose_a_reading():
    """The separator comes back as a dot or a semicolon often enough, and the
    label arrives wrapped in whatever debris was around it."""
    assert parse_times("21.35 |MSK|", ZONES)[0].minute == 35
    assert parse_times("21;35 .MSK.", ZONES)[0].minute == 35


@pytest.mark.parametrize(
    "text",
    [
        "24:00 MSK",  # not a time of day
        "21:75 MSK",  # not a minute
        "Proголосовали 12754 MSK",  # digits, but not a time
        "21:35",  # no zone, and none was configured
        "21:35 CET",  # a zone this caller did not name
        "2135 MSK",  # no separator
    ],
)
def test_what_is_not_a_reading(text):
    assert parse_times(text, ZONES) == []


def test_a_label_is_not_matched_inside_a_word():
    """`ET` is a real zone label and `TICKET` is a real word on a real overlay.
    A substring test would turn one into the other.

    Anchoring to the digits is not sufficient on its own, because folding strips
    the word boundaries: `ETA` after the time still starts with `ET`, and
    `TICKET` before it still ends with `ET`. Only the first case here is caught
    by position alone, which is what made the original test pass while both
    others were open.
    """
    et = {"ET": -5 * 3600}
    assert parse_times("11:20 TICKET", et) == []
    assert parse_times("11:20 ETA 5 min", et) == []
    assert parse_times("TICKET 11:20", et) == []
    assert parse_times("11:20 ET", et)[0].hour == 11
    assert parse_times("ET 11:20", et)[0].hour == 11


def test_a_letter_spaced_label_still_reads():
    """Why folding survives the boundary check: OCR returns `E T` for a
    letter-spaced badge, and a whole-token comparison would reject it."""
    et = {"ET": -5 * 3600}
    assert parse_times("11:20 E T", et)[0].zone == "ET"
    assert parse_times("11:20 (ET)", et)[0].zone == "ET"
    assert parse_times("11:20 M S K", {"MSK": 3 * 3600})[0].zone == "MSK"


def test_a_label_followed_by_another_word_still_reads():
    """The case that rules out checking the boundary on the folded window:
    `MSK LIVE` and `MSKVA` both continue with a letter once the space is folded
    out, and only the first is a badge. The boundary has to be read off the
    text as printed."""
    msk = {"MSK": 3 * 3600}
    assert parse_times("21:35 MSK LIVE", msk)[0].zone == "MSK"
    assert parse_times("LIVE MSK 21:35", msk)[0].zone == "MSK"
    assert parse_times("21:35 MSKVA", msk) == []


def test_an_unlabelled_badge_needs_the_caller_to_say_which_zone():
    """Some broadcasters print a bare clock. That is readable, but only once
    the caller has stated what zone it is in: this module will not pick one."""
    assert parse_times("21:35", ZONES) == []
    (parsed,) = parse_times("21:35", ZONES, default_zone="MSK")
    assert parsed.zone == "MSK"


def test_labels_fold_over_case_and_punctuation():
    zones = resolve_zones({"m.s.k": 3 * 3600})
    assert "MSK" in zones
    assert parse_times("21:35 MSK", zones)[0].zone == "MSK"


# --- the zone table is data ------------------------------------------------

def has_tzdata(name: str) -> bool:
    """Whether an IANA name resolves on this host.

    `clock.py` names a host with no tz database as a supported case, which is
    why the zone table accepts fixed offsets at all, so the IANA leg of these
    tests is skipped rather than failed there. `importorskip("zoneinfo")` does
    not cover it: the module imports fine and only the lookup raises.
    """
    try:
        resolve_zones({"_": name})
    except ValueError:
        return False
    return True


def test_a_zone_may_be_an_offset_a_name_or_a_tzinfo():
    zones = resolve_zones({"A": 3 * 3600, "C": timezone(timedelta(hours=-5))})
    at = datetime(2026, 1, 1, tzinfo=UTC)
    assert zones["A"].utcoffset(at) == timedelta(hours=3)
    assert zones["C"].utcoffset(at) == timedelta(hours=-5)
    if has_tzdata("UTC"):
        assert resolve_zones({"B": "UTC"})["B"].utcoffset(at) == timedelta(0)


def test_an_unresolvable_zone_is_an_error_not_a_guess():
    with pytest.raises(ValueError):
        resolve_zones({"XYZ": "Not/AZone"})


def test_a_default_zone_outside_the_table_is_refused():
    with pytest.raises(ValueError):
        ClockReader(zones=ZONES, ocr=lambda p: "", default_zone="CET")


# --- reading a file --------------------------------------------------------

@pytest.fixture
def frames(monkeypatch, tmp_path):
    """Script what each band of each sampled frame says, with no images.

    `install(text_at, frames=n, fps=f)` calls `text_at(region, timestamp)` for
    every band the reader asks for and returns one preprocessing pass; a list
    return value is taken as several passes, which is how the disagreement
    between the grey and thresholded passes is simulated.
    """

    def install(text_at, count=60, fps=0.5):
        listing = []
        for i in range(count):
            path = tmp_path / f"frame-{i:05d}.jpg"
            path.write_text(str(i))
            listing.append((i / fps, path))
        stamps = {path: timestamp for timestamp, path in listing}

        def read_band(ocr, frame, region, work):
            got = text_at(tuple(region), stamps[frame])
            return list(got) if isinstance(got, (list, tuple)) else [got]

        monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(listing))
        monkeypatch.setattr(overlay, "read_band", read_band)
        return listing

    return install


def ticking(anchor_s: float, zone: str = "MSK", band=BADGE, live_from=0.0):
    """A badge that ticks in real time from `anchor_s`, and background elsewhere."""

    def text_at(region, timestamp):
        if region != band or timestamp < live_from:
            return "lorem ipsum 53% 12754"
        return _badge(anchor_s + timestamp, zone)

    return text_at


def reader(**kwargs):
    kwargs.setdefault("zones", ZONES)
    kwargs.setdefault("ocr", lambda p: "")
    kwargs.setdefault("regions", (NOISE, BADGE))
    kwargs.setdefault("workers", 1)
    return ClockReader(**kwargs)


def test_the_anchor_is_the_wall_clock_at_video_zero(frames):
    """The whole point: a badge read at t=300 says what t=0 was."""
    frames(ticking(21 * 3600 + 30 * 60))
    anchor = reader().anchor(MEDIA)
    assert anchor is not None
    assert anchor.zone_label == "MSK"
    assert abs(anchor.offset_s - (21 * 3600 + 30 * 60)) <= 1.0
    assert anchor.wall_clock_at(600.0) is None  # no date supplied, so no instant


def test_the_clock_moves_the_band_lock_onto_the_badge(frames):
    """The reader searches bands on the caller's judgement, and this caller's
    judgement is "a time next to a zone I named". A ticker full of numbers is
    not that, which is why the lock has to end up on the badge and stay."""
    frames(ticking(21 * 3600 + 30 * 60))
    # One instance: `reader()` twice would run the overlay reader of the first
    # against the bound interpreter of a second, so the band counters would
    # land on an object this test discards.
    clock = reader()
    reads = [read for read, _ in clock.reader.read(MEDIA, clock._interpret)]
    assert {read.region for read in reads[-10:]} == {BADGE}


def test_footage_with_no_badge_yields_no_anchor(frames):
    """The common case. Most archive footage carries no clock, and inventing an
    anchor from whatever digits a corner held is the failure this module is
    built to avoid."""
    frames(lambda region, t: "lorem ipsum 53% | 12754 | 1-2")
    assert reader().anchor(MEDIA) is None


def test_one_reading_is_never_an_anchor(frames):
    """A single confident misread looks exactly like a single correct read."""

    def once(region, timestamp):
        if region == BADGE and timestamp == 0.0:
            return "21:35 MSK"
        return ""

    frames(once)
    assert reader().anchor(MEDIA) is None


def test_a_misread_among_agreeing_readings_is_dropped(frames):
    """OCR turned a 3 into a 5 on one frame. It is a plausible time with the
    right label, and nothing about the reading itself gives it away; only the
    other readings do."""
    truth = ticking(21 * 3600 + 30 * 60)

    def with_a_misread(region, timestamp):
        if region == BADGE and timestamp == 40.0:
            return "21:55 MSK"
        return truth(region, timestamp)

    frames(with_a_misread)
    anchor = reader().anchor(MEDIA)
    assert anchor is not None
    assert abs(anchor.offset_s - (21 * 3600 + 30 * 60)) <= 1.0
    assert not anchor.drift
    assert anchor.segment.count == anchor.readings_total - 1


def test_readings_that_imply_two_anchors_are_reported_not_averaged(frames):
    """A recording can be an edit of two takes, and a cut moves the anchor for
    everything after it. Averaging the two would produce an anchor that is
    wrong for both halves and looks fine."""
    early = ticking(21 * 3600 + 30 * 60)
    late = ticking(22 * 3600 + 30 * 60)

    def edited(region, timestamp):
        return (early if timestamp < 60 else late)(region, timestamp)

    frames(edited, count=80)
    anchor = reader().anchor(MEDIA)
    assert anchor is not None and anchor.drift
    assert len(anchor.segments) == 2 and len(anchor.drifted) == 1
    offsets = sorted(round(segment.offset_s) for segment in anchor.segments)
    assert offsets[1] - offsets[0] == 3600
    assert anchor.agreement < 1.0


def test_a_run_of_misreads_is_contested_not_drift(frames):
    """Measured on real footage: fourteen consecutive frames of a badge showing
    21:40 came back as 23:30, which is enough agreement to survive any
    corroboration count. What gives it away is that those frames sit inside a
    stretch of video another anchor already covers, and no second of video has
    two wall clocks. An edit moves the anchor forward; a misread argues with
    the anchor where it already applies."""
    truth = ticking(21 * 3600 + 30 * 60)

    def with_a_bad_run(region, timestamp):
        if region == BADGE and 60.0 <= timestamp <= 80.0:
            return _badge(23 * 3600 + 30 * 60 + timestamp)
        return truth(region, timestamp)

    frames(with_a_bad_run, count=80)
    anchor = reader().anchor(MEDIA)
    assert anchor is not None
    assert len(anchor.segments) == 2
    assert not anchor.drift and len(anchor.contested) == 1
    assert abs(anchor.offset_s - (21 * 3600 + 30 * 60)) <= 1.0


def test_a_citation_after_a_cut_can_ask_which_anchor_covers_it(frames):
    """The point of reporting an edit rather than averaging it: the second half
    of the file is still citable, through the anchor derived from the second
    half."""
    early = ticking(21 * 3600 + 30 * 60)
    late = ticking(22 * 3600 + 30 * 60)
    frames(lambda region, t: (early if t < 60 else late)(region, t), count=80)
    anchor = reader().anchor(MEDIA)
    assert anchor is not None
    covering = anchor.segment_for(120.0)
    assert covering is not None
    assert abs(covering.offset_s - (22 * 3600 + 30 * 60)) <= 1.0
    assert anchor.segment_for(10_000.0) is None  # no readings out there


# --- minute resolution -----------------------------------------------------

def test_a_minute_flip_pins_the_anchor_to_the_frame_interval(frames):
    """The badge only shows minutes, so one reading places the anchor anywhere
    in a 60 second window. Two readings either side of the minute flip narrow
    it to the sampling interval, and no code goes looking for the flip: it is
    what intersecting the readings' own windows does.
    """
    frames(ticking(21 * 3600 + 30 * 60 + 59), count=80, fps=1.0)
    anchor = reader(fps=1.0).anchor(MEDIA)
    assert anchor is not None
    assert anchor.uncertainty_s <= 0.5  # half of the 1s frame interval
    assert anchor.spread_s >= 59.0


def test_readings_that_never_straddle_a_flip_stay_a_minute_wide(frames):
    """The honest converse. Sampling every 60 seconds from a fixed phase reads
    the badge repeatedly and learns nothing new; the anchor is still only known
    to within the minute, and says so."""
    frames(ticking(21 * 3600 + 30 * 60), count=30, fps=1 / 60)
    anchor = reader(fps=1 / 60).anchor(MEDIA)
    assert anchor is not None
    assert anchor.spread_s == 0.0
    assert anchor.uncertainty_s == pytest.approx(30.0)


def test_spread_and_uncertainty_are_complements():
    """Spread is not error here, it is coverage of the badge's minute: every
    second of disagreement between readings is a second removed from the
    anchor's uncertainty."""
    reads = [
        ClockRead(timestamp_s=0.0, seconds_of_day=100 * 60, zone="MSK"),
        ClockRead(timestamp_s=50.0, seconds_of_day=100 * 60, zone="MSK"),
    ]
    (segment,) = clock._segments(reads, min_readings=2)
    assert segment.spread_s == pytest.approx(50.0)
    assert segment.uncertainty_s == pytest.approx(5.0)


def test_a_file_that_runs_past_midnight_is_one_anchor(frames):
    """Seconds of the day wrap and video time does not. Handled wrong, the
    readings before and after midnight look like two different anchors."""
    frames(ticking(clock.DAY_S - 30), count=60)
    anchor = reader().anchor(MEDIA)
    assert anchor is not None and not anchor.drift
    assert abs(anchor.offset_s - (clock.DAY_S - 30)) <= 1.0


# --- reconciling the preprocessing passes ----------------------------------

def test_passes_that_contradict_each_other_drop_the_frame(frames):
    """Both preprocessing passes are run because each reads frames the other
    cannot. When both read and they disagree, one of them is wrong and there is
    no way to tell which, so the frame is not used. The band still counts as a
    hit: the clock is plainly there, and the reader must not lose the band over
    a frame this refused."""
    truth = ticking(21 * 3600 + 30 * 60)

    def disagreeing(region, timestamp):
        if region == BADGE and timestamp in (40.0, 60.0):
            return [truth(region, timestamp), "23:59 MSK"]
        return truth(region, timestamp)

    frames(disagreeing)
    reading = reader()
    reads = reading.read_clock(MEDIA)
    anchor = reading.anchor_from(reads)
    assert anchor is not None and not anchor.drift
    assert anchor.conflicts == 2
    assert all(read.timestamp_s not in (40.0, 60.0) for read in reads)


def test_a_pass_that_reads_nothing_costs_nothing(frames):
    """The usual case on real footage: one pass sees the badge, the other
    returns an empty string. That is not a contradiction."""
    truth = ticking(21 * 3600 + 30 * 60)
    frames(lambda region, t: ["", truth(region, t)])
    anchor = reader().anchor(MEDIA)
    assert anchor is not None and anchor.conflicts == 0


# --- turning an anchor into an instant -------------------------------------

def test_no_date_means_no_epoch(frames):
    """The badge names a time of day and never a date. Supplying one is the
    caller's business, and until then this reports the time of day and nothing
    that pretends to be an instant."""
    frames(ticking(21 * 3600 + 30 * 60))
    anchor = reader().anchor(MEDIA)
    assert anchor is not None
    assert anchor.epoch_s is None and anchor.wall_clock_at(0.0) is None
    assert anchor.time_of_day_at(1800.0) == pytest.approx(22 * 3600, abs=1.0)


def test_a_date_turns_the_anchor_into_an_aware_instant(frames):
    frames(ticking(21 * 3600 + 30 * 60))
    anchor = reader(anchor_date=date(2026, 2, 3)).anchor(MEDIA)
    assert anchor is not None
    when = anchor.wall_clock_at(0.0)
    assert when is not None and when.tzinfo is not None
    # 21:30 in a UTC+3 zone is 18:30 UTC, whatever the reader's own zone is.
    assert when.astimezone(UTC).replace(second=0, microsecond=0) == datetime(
        2026, 2, 3, 18, 30, tzinfo=UTC
    )
    assert anchor.epoch_s == pytest.approx(when.timestamp())


def test_the_zone_is_applied_at_the_anchors_own_date(frames):
    """A named zone is not a fixed offset. Resolving it at today's date, or at
    the reader's, silently moves an anchor by an hour for half the year."""
    if not has_tzdata("America/New_York"):
        pytest.skip("no tz database on this host; fixed offsets cover that case")
    zones = {"ET": "America/New_York"}
    frames(ticking(12 * 3600, zone="ET"))
    winter = reader(zones=zones, anchor_date=date(2026, 1, 15)).anchor(MEDIA)
    summer = reader(zones=zones, anchor_date=date(2026, 7, 15)).anchor(MEDIA)
    assert winter is not None and summer is not None
    assert winter.wall_clock_at(0.0).utcoffset() == timedelta(hours=-5)
    assert summer.wall_clock_at(0.0).utcoffset() == timedelta(hours=-4)


def test_two_labels_are_two_claims_even_at_the_same_offset(frames):
    """Zones are what the badge said, not what they resolve to. A file whose
    badge changes label has changed what it is asserting, and that is reported
    rather than merged."""
    zones = {"MSK": 3 * 3600, "TRT": 3 * 3600}

    def relabelled(region, timestamp):
        zone = "MSK" if timestamp < 60 else "TRT"
        return ticking(21 * 3600 + 30 * 60, zone=zone)(region, timestamp)

    frames(relabelled, count=80)
    anchor = reader(zones=zones).anchor(MEDIA)
    assert anchor is not None
    assert {segment.zone for segment in anchor.segments} == {"MSK", "TRT"}


# --- configuration ---------------------------------------------------------

def test_the_reader_searches_small_corner_bands_by_default():
    """Measured, not stylistic: the badge that reads cleanly out of a corner
    box returns nothing out of a full-width band, because a wide crop buries a
    two-word island in empty picture."""
    assert reader(regions=None).regions == clock.CLOCK_REGIONS
    assert all(w <= 0.35 and h <= 0.2 for _x, _y, w, h in clock.CLOCK_REGIONS)


def test_the_page_segmentation_mode_is_the_sparse_one(monkeypatch):
    """psm 11 is the difference between reading the badge and not reading it,
    so the default has to reach the engine rather than being decorative."""
    seen = {}
    monkeypatch.setattr(
        overlay, "default_ocr", lambda psm=None, lang=None: seen.update(psm=psm) or (lambda p: "")
    )
    ClockReader(zones=ZONES)
    assert seen["psm"] == clock.CLOCK_PSM == 11


def test_the_ocr_language_stays_the_callers_choice(monkeypatch):
    """It has to be, and for a sharper reason than script coverage: the
    language model changes the *digits*. On the sample footage the Cyrillic
    model read a badge showing 21:35 as 21:55 while the Latin model, on the
    same pixels, read it correctly. Neither reading is detectably wrong on its
    own, so this module cannot arbitrate and must not silently pick."""
    monkeypatch.delenv(overlay.LANG_ENV, raising=False)
    assert reader().lang is None
    assert reader(lang="ces").lang == "ces"
    monkeypatch.setenv(overlay.LANG_ENV, "ces")
    assert reader().lang == "ces"


def test_the_clock_reader_knows_nothing_about_a_corpus():
    """Structural, because a docstring cannot be linted. It may know about the
    overlay reader; a roster, a sport or a fixed zone table would mean the
    generic half has leaked into it."""
    source = Path(clock.__file__).read_text(encoding="utf-8")
    tree = ast.parse(source)
    imported: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module:
            imported.add(node.module.split(".")[-1])
        elif isinstance(node, ast.Import):
            imported.update(alias.name.split(".")[0] for alias in node.names)
    assert not (imported & {"scorebug", "jersey", "faces", "playbyplay", "roster"})
