"""Scorebug overlay parsing, roster matching and cue shape.

Backend-free: no tesseract, no Pillow, no video. The OCR strings below are
verbatim tesseract output from the pilot fixture (a Nationals at Giants
broadcast), including its mistakes, so the parser is tested against what the
engine actually returns rather than against clean text.
"""

import json
import sys

import pytest

from dirstral_annotator.recognizers import scorebug
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.scorebug import (
    BATTER,
    LOOSE,
    PITCHER,
    UNKNOWN,
    ScorebugRecognizer,
    _name_index,
    _RegionSearch,
    match_name,
    parse_overlay,
)
from dirstral_annotator.roster import Roster

MEDIA = "game.mp4"
WHOLE = (0.0, 0.0, 1.0, 1.0)  # pin the region: these tests are about parsing


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:daylen-lile", "name": "Daylen Lile", "number": "4",
         "aliases": ["D. Lile", "Lile"], "mlbam_id": 1},
        {"id": "player:robbie-ray", "name": "Robbie Ray", "number": "38",
         "aliases": ["R. Ray", "Ray"], "mlbam_id": 2},
        {"id": "player:nasim-nunez", "name": "Nasim Nuñez", "number": "26",
         "aliases": ["N. Nuñez", "Nuñez"], "mlbam_id": 3},
        {"id": "player:drew-gilbert", "name": "Drew Gilbert", "number": "0",
         "aliases": ["Gilbert"], "mlbam_id": 4},
        {"id": "player:jung-hoo-lee", "name": "Jung Hoo Lee", "number": "51",
         "aliases": ["Lee"], "mlbam_id": 5},
        # two Smiths: the overlay shows a surname, which cannot separate them
        {"id": "player:dylan-smith", "name": "Dylan Smith", "number": "66",
         "aliases": ["Smith"], "mlbam_id": 6},
        {"id": "player:will-smith", "name": "Will Smith", "number": "13",
         "aliases": ["Smith"], "mlbam_id": 7},
    ]))
    return Roster.load(path)


@pytest.fixture
def index(roster):
    return _name_index(roster)


# --- overlay parsing -------------------------------------------------------

def test_batter_and_pitcher_fields_get_their_roles():
    names, _ = parse_overlay("5.LILE 1-2 RAY p: 87 “6 @@ 2-0")
    assert scorebug.NameRead("LILE", BATTER) in names
    assert scorebug.NameRead("RAY", PITCHER) in names


def test_day_line_digit_is_not_read_as_a_lineup_slot():
    # Verbatim OCR of a frame that used to make the pitcher the batter: the
    # "1-2." day line sits between the two names, offering "2. RAY".
    names, _ = parse_overlay('“5LILE 1-2. RAY me “6 00 1-0')
    assert scorebug.NameRead("RAY", BATTER) not in names
    assert scorebug.NameRead("LILE", BATTER) in names


def test_lineup_slot_survives_a_missing_separator():
    names, _ = parse_overlay("6YOUNG 0-2 RAY P: 90")
    assert scorebug.NameRead("YOUNG", BATTER) in names


def test_pitch_graphic_with_a_readable_unit():
    _, pitches = parse_overlay("5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0")
    assert pitches == [scorebug.PitchRead("SLIDER", 88, "MPH")]
    assert pitches[0].describe() == "SLIDER 88 MPH"


def test_pitch_graphic_when_the_unit_is_garbled():
    # "MPH" is set in small caps and comes back as noise more often than not;
    # the pitch type carries the read instead.
    _, pitches = parse_overlay("5.LILE 1-2 FOURSEAM 92upeq “G @@ 2-0")
    assert [(p.pitch_type, p.speed) for p in pitches] == [("FOURSEAM", 92)]


def test_implausible_speeds_and_non_pitches_are_not_graphics():
    _, pitches = parse_overlay("WSH 2 SF 0 ATTENDANCE 41213")
    assert pitches == []


def test_crowd_texture_is_never_more_than_a_loose_read():
    # A replay or crowd shot leaves no bug in the crop; tesseract still
    # returns pages of three-letter words, one of which is always a surname.
    noise = "Beir Lee GAS Pt atthe gL) TAT eM we PA) a pik wee Se bh\nFRR ahe ey Maes"
    names, pitches = parse_overlay(noise)
    assert pitches == []
    assert names and {n.role for n in names} == {LOOSE}


def test_bare_words_are_kept_only_alongside_structure():
    names, _ = parse_overlay("0-2 SEYMOUR 5.VIVAS")
    assert scorebug.NameRead("SEYMOUR", UNKNOWN) in names


# --- roster matching -------------------------------------------------------

def test_exact_surname_matches(index):
    assert match_name(index, "LILE") == ("player:daylen-lile", 0.95)


def test_accents_fold_both_ways(index):
    assert match_name(index, "NUNEZ")[0] == "player:nasim-nunez"


def test_ocr_debris_no_longer_resolves(index):
    # Every one of these produced a confident cue before: "ee" scored 0.80
    # against Lee, "inn" 0.86 against Winn, and "#0" matched the roster's
    # own "#number" alias at 1.00.
    for junk in ("ee", "12", "#0", "in", "e"):
        assert match_name(index, junk) is None


def test_short_tokens_need_an_exact_match(index):
    assert match_name(index, "RAY") == ("player:robbie-ray", 0.95)
    assert match_name(index, "RAM") is None  # one letter off, and too short to tell


def test_fuzzy_match_is_allowed_for_longer_names(index):
    hit = match_name(index, "GILBERI")
    assert hit is not None and hit[0] == "player:drew-gilbert"
    assert hit[1] < 0.95


def test_a_shared_surname_resolves_to_nobody(index):
    assert match_name(index, "SMITH") is None


# --- region search ---------------------------------------------------------

def test_search_sweeps_on_a_stride_then_locks():
    regions = (("a",), ("b",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    assert search.regions_for(0) == regions
    assert search.regions_for(1) == ()
    for _ in range(scorebug.LOCK_AFTER_READS):
        search.record(regions[1], hits=1)
    assert search.regions_for(0) == (regions[1],)


def test_a_single_region_is_locked_from_the_start():
    search = _RegionSearch((("only",),))  # type: ignore[arg-type]
    assert search.regions_for(1) == (("only",),)


def test_lock_releases_when_the_bug_moves():
    regions = (("a",), ("b",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    for _ in range(scorebug.LOCK_AFTER_READS):
        search.record(regions[0], hits=1)
    for _ in range(_RegionSearch.RELEASE_AFTER_MISSES):
        search.record(regions[0], hits=0)
    assert search.regions_for(0) == regions


# --- recognizer ------------------------------------------------------------

@pytest.fixture
def fake_frames(monkeypatch, tmp_path):
    """Drive the recognizer off a scripted list of per-frame OCR strings."""

    def install(texts, fps=0.5):
        frames = []
        for i, text in enumerate(texts):
            path = tmp_path / f"frame-{i}.jpg"
            path.write_text(text)
            frames.append((i / fps, path))

        monkeypatch.setattr(scorebug, "iter_frames", lambda *a, **k: iter(frames))
        # The real crop needs Pillow and a real JPEG; the fake hands the OCR
        # callable the frame itself, once per preprocessing pass.
        monkeypatch.setattr(
            scorebug, "_prepared_crops", lambda frame, region, work: iter([frame])
        )
        return lambda frame: frame.read_text()

    return install


def test_batter_becomes_an_at_bat_run_and_pitcher_an_appearance(roster, fake_frames):
    ocr = fake_frames(["5.LILE 1-2 RAY P: 87 “6 @@ 2-0"] * 4)
    cues = ScorebugRecognizer(roster, ocr=ocr, fps=0.5, crop=WHOLE).recognize(MEDIA)
    at_bat = [c for c in cues if c.event == "at_bat"]
    appearance = [c for c in cues if c.event == "appearance"]
    assert len(at_bat) == 1 and at_bat[0].entity_ids == ("player:daylen-lile",)
    assert at_bat[0].start_s == 0.0 and at_bat[0].end_s == 8.0
    assert len(appearance) == 1 and appearance[0].entity_ids == ("player:robbie-ray",)


def test_pitch_graphic_becomes_one_pitch_cue_for_the_pitcher(roster, fake_frames):
    ocr = fake_frames([
        "5.LILE 1-2 RAY P: 87 “6 @@ 2-0",
        "5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0",
        "5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0",  # same graphic, next frame
        "5.LILE 1-2 RAY P: 88 “6 @@ 2-1",
    ])
    pitches = [c for c in ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
               if c.event == "pitch"]
    assert len(pitches) == 1
    assert pitches[0].entity_ids == ("player:robbie-ray",)
    assert pitches[0].text == "SLIDER 88 MPH"
    assert pitches[0].start_s < 2.0 < pitches[0].end_s


def test_pitch_with_no_pitcher_in_sight_is_dropped(roster, fake_frames):
    ocr = fake_frames(["5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    assert [c for c in cues if c.event == "pitch"] == []


def test_pitch_cues_can_be_turned_off(roster, fake_frames):
    ocr = fake_frames(["RAY P: 87", "SLIDER 88 MPH"])
    rec = ScorebugRecognizer(roster, ocr=ocr, pitch_cues=False, crop=WHOLE)
    cues = rec.recognize(MEDIA)
    assert [c for c in cues if c.event == "pitch"] == []


def test_frames_without_a_bug_contribute_nothing(roster, fake_frames):
    # "Lee" is on the roster and is the surname tesseract hallucinates most;
    # with no structured read of him anywhere near, it stays dropped.
    ocr = fake_frames(["Beir Lee GAS Pt atthe", "FRR ahe ey Maes", ""])
    assert ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA) == []


def test_a_loose_read_is_admitted_once_confirmed(roster, fake_frames):
    ocr = fake_frames(["RAY P: 87 5.LILE", "a ray of ligh", "RAY P: 88 5.LILE"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    ray = [c for c in cues if c.entity_ids == ("player:robbie-ray",)]
    assert len(ray) == 1 and ray[0].start_s == 0.0 and ray[0].end_s == 6.0


def test_the_default_sweep_finds_the_bug_without_being_told_where(roster, fake_frames):
    # No crop given: the recognizer sweeps its candidate bands, locks onto the
    # one producing reads, and keeps reading once locked.
    ocr = fake_frames(["5.LILE 1-2 RAY P: 87 “6 @@ 2-0"] * 24)
    cues = ScorebugRecognizer(roster, ocr=ocr).recognize(MEDIA)
    at_bat = [c for c in cues if c.event == "at_bat"]
    assert at_bat and {c.entity_ids for c in at_bat} == {("player:daylen-lile",)}
    # Pre-lock sweeps are strided, so the early reads are spaced out and land
    # in separate runs; what matters is that reading continues past the lock.
    assert max(c.end_s for c in at_bat) > 40.0


def test_an_explicit_crop_pins_the_region(roster, fake_frames):
    fake_frames(["5.LILE 1-2 RAY P: 87"])
    rec = ScorebugRecognizer(roster, ocr=lambda p: "", crop=(0.0, 0.0, 0.5, 0.2))
    assert rec.regions == ((0.0, 0.0, 0.5, 0.2),)


def test_missing_ocr_backend_degrades_instead_of_aborting(monkeypatch):
    monkeypatch.setitem(sys.modules, "pytesseract", None)
    with pytest.raises(RecognizerUnavailable):
        scorebug.default_ocr()
