"""Play-by-play recognizer: role-aware event typing.

A pitch involves two rostered-relevant roles. The pitcher *threw* it (a
`pitch` event, which the pitcher-keyed phase-1 metric scores); the batter
*faced* it (an `at_bat` appearance, which must NOT enter the pitch pool). The
regression these tests lock down: a rostered batter facing an opponent's
pitch used to be emitted as a `pitch` cue, turning every opponent-pitched
at-bat into a precision false positive and flooring precision to ~50% even
with a perfect metadata source.
"""

import json
from pathlib import Path

import pytest

from dirstral_annotator.eval.align import Anchor, estimate
from dirstral_annotator.eval.ground_truth import PitchEvent
from dirstral_annotator.eval.score import score
from dirstral_annotator.fusion import fuse
from dirstral_annotator.recognizers.playbyplay import PlayByPlayRecognizer
from dirstral_annotator.roster import Roster

MEDIA = Path("game.mp4")  # unused by the recognizer: labels are external

# roster: one of our pitchers, one of our position players
WEBB = 657277
RAMOS = 671218
OPP_PITCHER = 111111
OPP_BATTER = 222222


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": WEBB},
        {"id": "player:ramos-heliot", "name": "Heliot Ramos", "number": "17", "mlbam_id": RAMOS},
    ]))
    return Roster.load(path)


def _pitch(epoch, pitcher, batter, pname="P", bname="B", desc="Ball"):
    return PitchEvent(
        game_pk=1, epoch_s=epoch, pitcher_id=pitcher, pitcher_name=pname,
        batter_id=batter, batter_name=bname, inning=1, description=desc,
    )


def test_rostered_pitcher_emits_pitch(roster):
    ev = _pitch(1000.0, WEBB, OPP_BATTER, pname="Logan Webb", bname="Opp Guy")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 1
    assert cues[0].event == "pitch"
    assert cues[0].entity_ids == ("player:webb-logan",)
    assert "Logan Webb" in cues[0].text


def test_rostered_batter_emits_at_bat_not_pitch(roster):
    # opponent pitching to our batter -> at_bat, NEVER pitch
    ev = _pitch(1000.0, OPP_PITCHER, RAMOS, pname="Opp Ace", bname="Heliot Ramos")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 1
    assert cues[0].event == "at_bat"
    assert cues[0].entity_ids == ("player:ramos-heliot",)
    assert "Heliot Ramos" in cues[0].text


def test_our_pitcher_facing_our_batter_splits_into_two_events(roster):
    # intrasquad edge: both rostered -> one pitch (pitcher) + one at_bat (batter)
    ev = _pitch(1000.0, WEBB, RAMOS)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 2  # exactly one pitch + one at_bat, no duplicates
    assert {(c.event, c.entity_ids) for c in cues} == {
        ("pitch", ("player:webb-logan",)),
        ("at_bat", ("player:ramos-heliot",)),
    }


def test_no_rostered_participant_skipped(roster):
    ev = _pitch(1000.0, OPP_PITCHER, OPP_BATTER)
    assert PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA) == []


def test_batter_appearance_is_not_a_precision_false_positive(roster):
    # THE REGRESSION: a game that is half our-pitcher, half our-batter. The
    # at-bats must not be scored as pitch false positives -> precision stays
    # 1.0, not ~0.5.
    events = [
        _pitch(1000.0, WEBB, OPP_BATTER, pname="Logan Webb"),        # our pitch
        _pitch(1100.0, OPP_PITCHER, RAMOS, bname="Heliot Ramos"),    # our at-bat
        _pitch(1200.0, WEBB, OPP_BATTER, pname="Logan Webb"),        # our pitch
        _pitch(1300.0, OPP_PITCHER, RAMOS, bname="Heliot Ramos"),    # our at-bat
    ]
    alignment = estimate([Anchor(events[0].epoch_s, 5.0)])
    cues = PlayByPlayRecognizer(events, alignment.offset_s, roster).recognize(MEDIA)
    anns = fuse(cues)
    card = score(anns, events, alignment, roster)
    # only Webb's two pitches are rostered-pitcher events
    assert card.total_events == 2
    assert card.overall.recall == 1.0
    assert card.overall.precision == 1.0  # at-bats stay out of the pitch pool
