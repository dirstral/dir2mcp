"""Scorebug interpretation: overlay parsing, roster matching and cue shape.

This is the baseball half of the seam. The reading half (region search,
preprocessing, the pool, the OCR adapter) is generic and lives in
`test_overlay.py`; what is pinned here is what the strings *mean*.

Backend-free: no tesseract, no Pillow, no video. The OCR strings below are
verbatim tesseract output from the pilot fixture (a Nationals at Giants
broadcast), including its mistakes, so the parser is tested against what the
engine actually returns rather than against clean text.
"""

import json
import sys

import pytest

from dirstral_annotator.recognizers import base, overlay, scorebug
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.overlay import OverlayRead
from dirstral_annotator.recognizers.scorebug import (
    BATTER,
    LOOSE,
    PITCHER,
    UNKNOWN,
    ScorebugRecognizer,
    _name_index,
    match_name,
    parse_bands,
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
    names, _, _ = parse_overlay("5.LILE 1-2 RAY p: 87 “6 @@ 2-0")
    assert scorebug.NameRead("LILE", BATTER) in names
    assert scorebug.NameRead("RAY", PITCHER) in names


def test_day_line_digit_is_not_read_as_a_lineup_slot():
    # Verbatim OCR of a frame that used to make the pitcher the batter: the
    # "1-2." day line sits between the two names, offering "2. RAY".
    names, _, _ = parse_overlay('“5LILE 1-2. RAY me “6 00 1-0')
    assert scorebug.NameRead("RAY", BATTER) not in names
    assert scorebug.NameRead("LILE", BATTER) in names


def test_lineup_slot_survives_a_missing_separator():
    names, _, _ = parse_overlay("6YOUNG 0-2 RAY P: 90")
    assert scorebug.NameRead("YOUNG", BATTER) in names


def test_pitch_graphic_with_a_readable_unit():
    _, pitches, _ = parse_overlay("5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0")
    assert pitches == [scorebug.PitchRead("SLIDER", 88, "MPH")]
    assert pitches[0].describe() == "SLIDER 88 MPH"


def test_pitch_graphic_when_the_unit_is_garbled():
    # "MPH" is set in small caps and comes back as noise more often than not;
    # the pitch type carries the read instead.
    _, pitches, _ = parse_overlay("5.LILE 1-2 FOURSEAM 92upeq “G @@ 2-0")
    assert [(p.pitch_type, p.speed) for p in pitches] == [("FOURSEAM", 92)]


def test_implausible_speeds_and_non_pitches_are_not_graphics():
    _, pitches, _ = parse_overlay("WSH 2 SF 0 ATTENDANCE 41213")
    assert pitches == []


def test_crowd_texture_is_never_more_than_a_loose_read():
    # A replay or crowd shot leaves no bug in the crop; tesseract still
    # returns pages of three-letter words, one of which is always a surname.
    noise = "Beir Lee GAS Pt atthe gL) TAT eM we PA) a pik wee Se bh\nFRR ahe ey Maes"
    names, pitches, _ = parse_overlay(noise)
    assert pitches == []
    assert names and {n.role for n in names} == {LOOSE}


def test_bare_words_are_kept_only_alongside_structure():
    names, _, _ = parse_overlay("0-2 SEYMOUR 5.VIVAS")
    assert scorebug.NameRead("SEYMOUR", UNKNOWN) in names


def test_both_preprocessing_passes_are_merged_not_chosen_between():
    """Neither pass is reliably better, so the union of the two is kept: 44 of
    215 readable pilot frames were readable only after thresholding."""
    names, pitches, _ = parse_bands([
        "5.LILE 1-2 RAY P: 87",          # the grey pass lost the pitch graphic
        "5.LILE 1-2 SLIDER 88 MPH",      # the binarised pass lost the pitcher
    ])
    assert scorebug.NameRead("LILE", BATTER) in names
    assert scorebug.NameRead("RAY", PITCHER) in names
    assert [(p.pitch_type, p.speed) for p in pitches] == [("SLIDER", 88)]
    # A name both passes agree on is one read, not two.
    assert [n for n in names if n.name == "LILE"] == [scorebug.NameRead("LILE", BATTER)]


def test_a_speed_read_twice_keeps_the_fuller_description():
    _, pitches, _ = parse_bands(["SLIDER 88", "SLIDER 88 MPH"])
    assert [p.describe() for p in pitches] == ["SLIDER 88 MPH"]


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


# --- the evidence handed back to the band search ---------------------------

def _hits(roster, text):
    rec = ScorebugRecognizer(roster, ocr=lambda p: "", crop=WHOLE)
    _, hits = rec._interpret(OverlayRead(0, 0.0, WHOLE, (text,)))
    return hits


def test_only_roster_reads_off_a_structured_line_count_as_evidence(roster):
    """The reader locks its band search on this number, and the number is the
    reason the search lands on the bug rather than on the stands. "The OCR
    returned something" would not do: crowd texture returns plenty."""
    assert _hits(roster, "5.LILE 1-2 RAY P: 87") == 2  # batter and pitcher
    assert _hits(roster, "5.LILE 1-2 SLIDER 88 MPH") == 2  # batter and a graphic


def test_crowd_texture_is_worth_nothing_to_the_band_search(roster):
    # Verbatim from a replay: plenty of text, one word of it a real surname.
    noise = "Beir Lee GAS Pt atthe gL) TAT eM we PA) a pik wee Se bh"
    fields, hits = ScorebugRecognizer(
        roster, ocr=lambda p: "", crop=WHOLE
    )._interpret(OverlayRead(0, 0.0, WHOLE, (noise,)))
    assert hits == 0, "a loose read must never vote for a band"
    assert [role for _, _, role in fields.names] == [LOOSE]


def test_a_name_nobody_on_the_roster_shares_is_worth_nothing(roster):
    assert _hits(roster, "5.BONDS 1-2 MAYS P: 87") == 0


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

        # Frame sampling and preprocessing belong to the reader now, so the
        # fakes are installed there.
        monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(frames))
        # The real crop needs Pillow and a real JPEG; the fake hands the OCR
        # callable the frame itself, once per preprocessing pass.
        monkeypatch.setattr(
            overlay, "_prepared_crops", lambda frame, region, work: iter([frame])
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
    # The appearance run is what this test is about. The fixture also steps the
    # pitch count 87 -> 88, so it now yields a `pitch` cue as well; that is
    # asserted on its own below rather than folded in here.
    ray = [c for c in cues
           if c.entity_ids == ("player:robbie-ray",) and c.event == "appearance"]
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


def test_velocity_branch_rejects_implausible_speeds():
    """The unit-anchored pattern must apply the same plausibility gate as the
    typed one; a garbled line ending in digits plus a unit was accepted at any
    value."""
    from dirstral_annotator.recognizers import scorebug as sb

    _, ok, _ = sb.parse_overlay("SLIDER 88 MPH")
    assert [p.speed for p in ok] == [88]

    for line in ("GARBLE 07 MPH", "NOISE 999 KPH", "XX 12 MPH"):
        _, bad, _ = sb.parse_overlay(line)
        assert bad == [], f"accepted implausible speed: {line} -> {bad}"


def test_the_names_other_modules_import_from_here_still_resolve():
    """`collapse_sightings`, `OcrFn` and `default_ocr` moved to `base` and
    `overlay` with the split; jersey, faces and callers outside the package
    have always imported them from here."""
    assert scorebug.collapse_sightings is base.collapse_sightings
    assert scorebug.default_ocr is overlay.default_ocr
    assert scorebug.default_workers is overlay.default_workers
    assert scorebug.OcrFn is overlay.OcrFn


# --- pitch cues from the bug's own pitch count ------------------------------

def test_a_count_step_is_a_pitch(roster, fake_frames):
    """The speed graphic is shown for some pitches and not others; on the pilot
    game it yielded 299 cues against 344 thrown. The count printed beside the
    pitcher's name is on the bug continuously and rises by exactly one per
    pitch, so a transition is a pitch with a timestamp, already attributed."""
    # Each count twice: the bug holds it for the whole at-bat, and a new value
    # has to survive COUNT_STABLE_READS frames before it is believed.
    ocr = fake_frames(["RAY P: 87", "RAY P: 87",
                       "RAY P: 88", "RAY P: 88",
                       "RAY P: 89", "RAY P: 89"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    pitches = [c for c in cues if c.event == "pitch"]
    assert len(pitches) == 2  # 87->88 and 88->89; the first read is a baseline
    assert all(c.entity_ids == ("player:robbie-ray",) for c in pitches)


def test_the_first_count_seen_claims_nothing(roster, fake_frames):
    """A single reading says a pitcher has thrown N pitches, not that he threw
    one just now. Only a transition is evidence of timing."""
    ocr = fake_frames(["RAY P: 87", "RAY P: 87", "RAY P: 87"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    assert [c for c in cues if c.event == "pitch"] == []


def test_a_jump_larger_than_one_says_nothing(roster, fake_frames):
    """Pitches certainly happened, but WHEN is unknown: emitting them at the
    moment the jump was noticed would invent timing the bug never showed. This
    is also the OCR-noise guard, since a misread digit looks exactly like a
    jump."""
    ocr = fake_frames(["RAY P: 87", "RAY P: 93"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    assert [c for c in cues if c.event == "pitch"] == []


def test_a_count_that_goes_backwards_resets_rather_than_emits(roster, fake_frames):
    """A new pitcher starts his own count, and OCR misreads a digit downward.
    Neither is a pitch."""
    ocr = fake_frames(["RAY P: 87", "RAY P: 87",
                       "RAY P: 12", "RAY P: 12",
                       "RAY P: 13", "RAY P: 13"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    pitches = [c for c in cues if c.event == "pitch"]
    assert len(pitches) == 1 and pitches[0].text == "pitch 13"


def test_a_pitch_a_graphic_already_reported_is_not_reported_twice(roster, fake_frames):
    """The scorecard matches at most one annotation per ground-truth pitch and
    counts the rest as false positives, so a duplicate cannot raise recall and
    can only cost precision — which is currently perfect on the pilot."""
    ocr = fake_frames(["RAY P: 87", "RAY P: 88 SLIDER 88 MPH"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    pitches = [c for c in cues if c.event == "pitch"]
    assert len(pitches) == 1, f"the same pitch was reported twice: {pitches}"


def test_count_pitches_can_be_turned_off_with_the_graphic_ones(roster, fake_frames):
    ocr = fake_frames(["RAY P: 87", "RAY P: 88"])
    rec = ScorebugRecognizer(roster, ocr=ocr, pitch_cues=False, crop=WHOLE)
    assert [c for c in rec.recognize(MEDIA) if c.event == "pitch"] == []


def test_a_single_frame_misread_does_not_manufacture_a_pitch(roster, fake_frames):
    """The measured failure of the first version of this feature.

    A +1 step is not only what a real pitch looks like, it is also the most
    likely OCR error, so one misread digit produced 87 -> 88 (a phantom pitch),
    then 88 -> 87, then 87 -> 88 again: three readings, two phantom pitches, no
    pitch thrown. On the pilot game that pattern turned 52 new cues into only 17
    credited pitches and cost 7.6 points of precision.

    A transient value that does not survive the next reading is now discarded.
    """
    ocr = fake_frames(["RAY P: 87", "RAY P: 87", "RAY P: 88",
                       "RAY P: 87", "RAY P: 87", "RAY P: 87"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    assert [c for c in cues if c.event == "pitch"] == []


def test_a_real_step_survives_a_neighbouring_misread(roster, fake_frames):
    """The other half: noise must not suppress a genuine pitch either."""
    ocr = fake_frames(["RAY P: 87", "RAY P: 87", "RAY P: 99",
                       "RAY P: 88", "RAY P: 88", "RAY P: 88"])
    cues = ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE).recognize(MEDIA)
    pitches = [c for c in cues if c.event == "pitch"]
    assert len(pitches) == 1 and pitches[0].text == "pitch 88"
