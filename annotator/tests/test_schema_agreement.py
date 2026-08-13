"""Pin backend ↔ spec agreement: the response `serve` returns must validate
against the draft wire-contract schema shipped with dirstral-spec design
0004. Skipped only when the schema (submodule) isn't checked out; a missing
`jsonschema` is a hard error (install the `test` extra), not a silent skip —
otherwise this contract test would pass without validating anything.

Override the schema location with DIRSTRAL_SPEC_DIR when working from a
side-by-side spec checkout instead of the submodule.
"""

import os
from pathlib import Path

import jsonschema
import pytest

from dirstral_annotator.emit import build_response
from dirstral_annotator.eval import ground_truth
from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Annotation, Document, Player, slug_entity_id
from dirstral_annotator.recognizers.playbyplay import NOTABILITY_EVENTS, PlayByPlayRecognizer
from dirstral_annotator.roster import Roster

SCHEMA_REL = "docs/design/0004-recognize-response.schema.json"


def find_schema() -> Path | None:
    candidates = []
    if os.environ.get("DIRSTRAL_SPEC_DIR"):
        candidates.append(Path(os.environ["DIRSTRAL_SPEC_DIR"]) / SCHEMA_REL)
    repo_root = Path(__file__).resolve().parents[2]
    candidates.append(repo_root / "dirstral-spec" / SCHEMA_REL)  # submodule
    candidates.append(repo_root.parent / "dirstral-spec" / SCHEMA_REL)  # sibling checkout
    return next((c for c in candidates if c.is_file()), None)


def test_serve_response_validates_against_design_0004_draft_schema():
    schema_path = find_schema()
    if schema_path is None:
        pytest.skip("dirstral-spec design-0004 draft schema not available")
    import json

    doc = Document(
        media="game7.mp4",
        annotations=[
            Annotation(
                start_s=2530.0, end_s=2551.0, event="pitch",
                entity_ids=("player:webb-logan",),
                text="Pitch: Logan Webb to Freddie Freeman — fly out",
                confidence=0.97, sources=("playbyplay", "scorebug"),
            )
        ],
    )
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    jsonschema.validate(build_response(doc), schema)


def test_the_notability_cues_are_legal_wire():
    """The whole pilot game, through the real path, against the same schema.

    `annotations[]` is closed (`additionalProperties: false`), so the contract
    has no numeric field: a captivating index or an exit velocity can only reach
    a client inside `text`, and a fact can only be selected on through `event`,
    which the contract leaves as a "producer-defined event vocabulary". This
    validates that the cues carrying them are legal wire, and it fails the day
    somebody answers "where does the number go?" by inventing a field here
    instead of proposing one in dirstral-spec.
    """
    schema_path = find_schema()
    if schema_path is None:
        pytest.skip("dirstral-spec design-0004 draft schema not available")
    import json

    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    feed = ground_truth.load_game(Path(__file__).parent / "fixtures" / "gumbo_823215.json")
    events = ground_truth.parse_pitches(feed)
    players, mlbam = [], {}
    for ev in events:
        for pid, name in ((ev.pitcher_id, ev.pitcher_name), (ev.batter_id, ev.batter_name)):
            if pid and pid not in mlbam:
                player = Player(id=slug_entity_id(name), name=name)
                mlbam[pid] = player.id
                players.append(player)
    roster = Roster(players, mlbam)

    cues = PlayByPlayRecognizer(events, 0.0, roster).recognize(Path("game.mp4"))
    annotations = fuse(cues)
    doc = Document(media="game.mp4", annotations=annotations)
    payload = build_response(doc, roster)
    jsonschema.validate(payload, schema)

    emitted = {a["event"] for a in payload["annotations"]}
    assert set(NOTABILITY_EVENTS).issubset(emitted), emitted
