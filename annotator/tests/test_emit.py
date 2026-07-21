import json

from dirstral_annotator.emit import emit_json, emit_vtt, vtt_timestamp, write_sidecars
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


def test_vtt_timestamp():
    assert vtt_timestamp(0) == "00:00:00.000"
    assert vtt_timestamp(2530.0) == "00:42:10.000"
    assert vtt_timestamp(3661.5) == "01:01:01.500"


def test_vtt_matches_design_0004_convention():
    out = emit_vtt(doc())
    assert out.startswith("WEBVTT\n\n")
    assert "00:42:10.000 --> 00:42:31.000" in out
    assert "Pitch: Logan Webb to Freddie Freeman — fly out [sources: face+scorebug; confidence 0.97]" in out


def test_json_shape():
    payload = json.loads(emit_json(doc()))
    assert payload["media"] == "game7.mp4"
    assert payload["annotator"]["name"] == "dirstral-sports-annotator"
    assert payload["entities"][0]["id"] == "player:webb-logan"
    ann = payload["annotations"][0]
    assert ann["start_s"] == 2530.0 and ann["end_s"] == 2551.0
    assert ann["entities"] == ["player:webb-logan"]
    assert ann["sources"] == ["face", "scorebug"]
    assert 0 <= ann["confidence"] <= 1


def test_write_sidecars_paths(tmp_path):
    media = tmp_path / "game7.mp4"
    media.write_bytes(b"")
    written = write_sidecars(doc(), media)
    assert [p.name for p in written] == ["game7.vtt", "game7.annotations.json"]
    assert (tmp_path / "game7.vtt").read_text().startswith("WEBVTT")
    json.loads((tmp_path / "game7.annotations.json").read_text())
