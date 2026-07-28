import json

from dirstral_annotator.emit import build_response, response_json
from dirstral_annotator.model import Annotation, Document


def doc():
    return Document(
        media="game7.mp4",
        annotations=[
            Annotation(
                start_s=2530.0, end_s=2551.0, event="pitch",
                entity_ids=("player:webb-logan",),
                text="Pitch: Logan Webb to Freddie Freeman — fly out",
                confidence=0.97, sources=("face", "scorebug"),
            )
        ],
    )


def test_response_matches_design_0004_wire_contract():
    payload = build_response(doc())
    # the identity dir2mcp folds into the representation's derivation identity
    assert payload["recognizer"] == {"name": "dirstral-annotator", "version": "0.2.0"}
    assert payload["entities"][0]["id"] == "player:webb-logan"
    ann = payload["annotations"][0]
    assert ann["start_s"] == 2530.0 and ann["end_s"] == 2551.0
    assert ann["entities"] == ["player:webb-logan"]
    assert ann["sources"] == ["face", "scorebug"]
    assert ann["event"] == "pitch"
    assert 0 <= ann["confidence"] <= 1


def test_response_json_round_trips():
    assert json.loads(response_json(doc())) == build_response(doc())
