"""Every cue a frame-sampling recognizer builds must carry text (#899).

The wire contract requires it: design 0004's response schema declares
`text` with `minLength: 1` and `pattern: "\\S"`. dir2mcp enforces the same
thing from the other side and drops any annotation whose text is blank
(`internal/ingest/recognize.go`).

`collapse_sightings` used to build every cue with no text at all. Nothing
noticed, because the only annotations anyone validated came from
play-by-play, which writes its own sentences. The recognizers that go
through `collapse_sightings` were computed in full and then discarded before
storage: face and jersey contributed zero retrievable spans, and scorebug's
`at_bat` cues survived only where play-by-play happened to name the same
at_bat and lend them its words.

These tests exercise the case the old ones missed: a document whose
annotations come from sightings ALONE, with no feed to borrow text from.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import jsonschema
import pytest

from dirstral_annotator.emit import build_response
from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Document, Player
from dirstral_annotator.recognizers.base import appearance_text, collapse_sightings
from dirstral_annotator.roster import Roster, display_name

SCHEMA_REL = "docs/design/0004-recognize-response.schema.json"

LEE = "player:jung-hoo-lee"
DEVERS = "player:rafael-devers"


def roster() -> Roster:
    return Roster([
        Player(id=LEE, name="Jung Hoo Lee", number="51", aliases=()),
        Player(id=DEVERS, name="Rafael Devers", number="11", aliases=()),
    ])


def find_schema() -> Path | None:
    candidates = []
    if os.environ.get("DIRSTRAL_SPEC_DIR"):
        candidates.append(Path(os.environ["DIRSTRAL_SPEC_DIR"]) / SCHEMA_REL)
    root = Path(__file__).resolve().parents[2]
    candidates.append(root / "dirstral-spec" / SCHEMA_REL)
    candidates.append(root.parent / "dirstral-spec" / SCHEMA_REL)
    return next((c for c in candidates if c.is_file()), None)


def appearance_cues():
    """Two runs of one player plus one of another, as a face pass would see."""
    sightings = [
        (10.0, LEE, 0.61), (10.5, LEE, 0.66), (11.0, LEE, 0.58),
        (90.0, LEE, 0.52),
        (200.0, DEVERS, 0.71), (200.5, DEVERS, 0.69),
    ]
    return collapse_sightings(
        sightings, source="face", event="appearance", frame_gap=0.5,
        describe=lambda pid: appearance_text(display_name(roster(), pid)),
    )


def test_appearance_only_document_is_legal_wire():
    """The regression. Before #899 every annotation here had text "" and the
    schema rejected the payload on minLength, which is the same condition
    ingest drops on."""
    schema_path = find_schema()
    if schema_path is None:
        pytest.skip("dirstral-spec design-0004 draft schema not available")
    doc = Document(media="game.mp4", annotations=fuse(appearance_cues()))
    assert doc.annotations, "no annotations to validate"
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    jsonschema.validate(build_response(doc, roster()), schema)


def test_every_appearance_annotation_has_text_naming_the_player():
    """Non-empty is the contract; naming the player is the point. A chunk is
    indexed on its text, so "on screen" alone would be unsearchable."""
    for ann in fuse(appearance_cues()):
        assert ann.text.strip(), f"blank text on {ann}"
        assert len(ann.entity_ids) == 1
        assert display_name(roster(), ann.entity_ids[0]) in ann.text


def test_scorebug_at_bat_survives_without_a_feed_to_borrow_text_from():
    """#739 calls the no-feed path the capability that transfers to archives.
    A scorebug at_bat cue must therefore stand on its own words."""
    cues = collapse_sightings(
        [(5.0, DEVERS, 0.8), (5.5, DEVERS, 0.8)],
        source="scorebug", event="at_bat", frame_gap=0.5,
        describe=lambda pid: f"{display_name(roster(), pid)} at bat",
    )
    anns = fuse(cues)
    assert len(anns) == 1
    assert anns[0].text == "Rafael Devers at bat"
    assert anns[0].sources == ("scorebug",)


def test_describe_is_required_so_the_drop_cannot_return_by_omission():
    """No default. A default would let the next frame-sampling recognizer
    reintroduce blank text silently, which is exactly how #899 happened."""
    with pytest.raises(TypeError):
        collapse_sightings(
            [(1.0, LEE, 0.5)], source="face", event="appearance", frame_gap=0.5,
        )


def test_display_name_falls_back_to_the_slug_for_an_unrostered_id():
    """A sighting can name someone the roster does not carry. The text still
    has to be non-blank, or the annotation is dropped for a reason that has
    nothing to do with the recognizer."""
    assert display_name(roster(), "player:matt-chapman") == "Matt Chapman"
    assert display_name(None, "player:matt-chapman") == "Matt Chapman"


def test_emit_and_recognizers_agree_on_what_a_player_is_called():
    """emit.build_response used to repeat the fallback rule inline. Two copies
    of a display rule drift; this pins that they are one rule."""
    doc = Document(media="game.mp4", annotations=fuse(appearance_cues()))
    payload = build_response(doc, roster())
    labels = {e["id"]: e["label"] for e in payload["entities"]}
    for ann in payload["annotations"]:
        for pid in ann["entities"]:
            assert labels[pid] in ann["text"]
