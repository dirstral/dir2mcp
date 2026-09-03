"""Inning attributes: the structured scope a filter can require (SPEC §9.10).

The half-inning already rides in the cue text, which lets an inning be
PREFERRED by similarity — and rank a talkative 7th above a quiet 8th (#928
measured exactly that). These tests pin the structured form: every play-by-
play cue of an event carries `{"inning": "8", "half": "bottom"}` in the
canonical forms dir2mcp compares byte-for-byte, fusion keeps only what every
stating cue agreed on, and the wire omits the map entirely when it is empty so
a pre-attributes annotation still hashes identically (§8.6.7).
"""

import json
from pathlib import Path

import pytest

from dirstral_annotator.emit import build_response
from dirstral_annotator.eval.ground_truth import PitchEvent
from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Annotation, Cue, Document
from dirstral_annotator.recognizers.playbyplay import PlayByPlayRecognizer
from dirstral_annotator.roster import Roster

MEDIA = Path("game.mp4")
WEBB = 657277

# Same resolution as test_schema_agreement (tests are not a package, so the
# few lines are restated rather than imported).
SCHEMA_REL = "docs/design/0004-recognize-response.schema.json"


def find_schema() -> Path | None:
    import os

    candidates = []
    if os.environ.get("DIRSTRAL_SPEC_DIR"):
        candidates.append(Path(os.environ["DIRSTRAL_SPEC_DIR"]) / SCHEMA_REL)
    repo_root = Path(__file__).resolve().parents[2]
    candidates.append(repo_root / "dirstral-spec" / SCHEMA_REL)
    candidates.append(repo_root.parent / "dirstral-spec" / SCHEMA_REL)
    return next((c for c in candidates if c.is_file()), None)


RAMOS = 671218


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": WEBB},
        {"id": "player:ramos-heliot", "name": "Heliot Ramos", "number": "17",
         "mlbam_id": RAMOS},
    ]))
    return Roster.load(path)


def _event(inning, top, batter=222222, **kw):
    return PitchEvent(
        game_pk=1, epoch_s=1000.0, pitcher_id=WEBB, pitcher_name="Logan Webb",
        batter_id=batter, batter_name="Opp Guy", inning=inning, top_inning=top,
        description="Ball", **kw,
    )


def test_every_cue_of_an_event_carries_the_inning(roster):
    # A rostered batter, so the event emits its FULL set: pitch, at_bat, the
    # outcome, and all four notability cues. Anything short of that and a cue
    # path could ship without the attributes (a mutation proved the gap when
    # this test used an unrostered batter and only ever saw the pitch cue).
    ev = _event(8, False, batter=RAMOS, event_type="home_run",
                is_scoring_play=True, rbi=1, away_score=2, home_score=3,
                has_review=True, captivating_index=50,
                hit_data=None)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    got_events = sorted(c.event for c in cues)
    assert got_events == sorted(
        ["pitch", "at_bat", "home_run", "captivating", "reviewed", "scoring_play"]
    ), got_events
    for cue in cues:
        assert cue.attributes == {"inning": "8", "half": "bottom"}, cue.event


def test_the_canonical_forms_are_unpadded_and_lowercase(roster):
    cues = PlayByPlayRecognizer([_event(10, True)], 0.0, roster).recognize(MEDIA)
    assert cues[0].attributes == {"inning": "10", "half": "top"}


def test_an_event_without_an_inning_states_no_scope(roster):
    cues = PlayByPlayRecognizer([_event(0, True)], 0.0, roster).recognize(MEDIA)
    assert cues and cues[0].attributes == {}


def test_fusion_keeps_attributes_every_stating_cue_agrees_on():
    base = dict(start_s=10.0, end_s=18.0, event="pitch",
                entity_ids=("player:webb-logan",), confidence=0.9)
    anns = fuse([
        Cue(source="playbyplay", text="Pitch", attributes={"inning": "8", "half": "top"}, **base),
        Cue(source="scorebug", text="", attributes={"inning": "8"}, **base),
    ])
    assert len(anns) == 1
    assert anns[0].attributes == {"inning": "8", "half": "top"}


def test_fusion_drops_a_key_the_cues_disagree_on():
    base = dict(start_s=10.0, end_s=18.0, event="pitch",
                entity_ids=("player:webb-logan",), confidence=0.9)
    anns = fuse([
        Cue(source="playbyplay", text="Pitch", attributes={"inning": "8", "half": "top"}, **base),
        Cue(source="scorebug", text="", attributes={"inning": "9", "half": "top"}, **base),
    ])
    assert len(anns) == 1
    # The disagreed inning is gone; the agreed half survives. Half a truth is
    # better than a coin flip the server would drop the whole annotation for.
    assert anns[0].attributes == {"half": "top"}


def _doc(attributes):
    return Document(media="game.mp4", annotations=[
        Annotation(start_s=1.0, end_s=2.0, event="pitch",
                   entity_ids=("player:webb-logan",), text="Pitch",
                   confidence=0.9, sources=("playbyplay",),
                   attributes=attributes),
    ])


def test_the_wire_omits_an_empty_attributes_map():
    ann = build_response(_doc({}))["annotations"][0]
    # Omitted, not `{}`: an absent map keeps the payload byte-identical to a
    # pre-attributes response, so dir2mcp's derivation hash does not re-derive
    # annotations that gained nothing (§8.6.7).
    assert "attributes" not in ann


def test_the_wire_carries_attributes_sorted():
    ann = build_response(_doc({"inning": "8", "half": "bottom"}))["annotations"][0]
    assert ann["attributes"] == {"half": "bottom", "inning": "8"}
    assert list(ann["attributes"]) == ["half", "inning"]


def test_an_attributed_response_is_legal_wire():
    schema_path = find_schema()
    if schema_path is None:
        pytest.skip("dirstral-spec design-0004 draft schema not available")
    import jsonschema

    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    jsonschema.validate(build_response(_doc({"inning": "8", "half": "bottom"})), schema)
    # And the reserved prefix is illegal wire, which proves the checked-out
    # schema is the 0.59.0 one and the propertyNames guard is live.
    with pytest.raises(jsonschema.ValidationError):
        jsonschema.validate(build_response(_doc({"dir2mcp:x": "1"})), schema)
