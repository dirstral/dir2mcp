"""Pin emitter ↔ spec agreement: the v1 JSON we emit must validate against
the draft schema shipped with dirstral-spec design 0004. Skipped when the
schema (submodule) or jsonschema isn't available; CI with the submodule
checked out runs it.

Override the schema location with DIRSTRAL_SPEC_DIR when working from a
side-by-side spec checkout instead of the submodule.
"""

import json
import os
from pathlib import Path

import pytest

from dirstral_annotator.emit import emit_json
from dirstral_annotator.model import Annotation, Document

SCHEMA_REL = "docs/design/0004-annotation-sidecar.schema.json"


def find_schema() -> Path | None:
    candidates = []
    if os.environ.get("DIRSTRAL_SPEC_DIR"):
        candidates.append(Path(os.environ["DIRSTRAL_SPEC_DIR"]) / SCHEMA_REL)
    repo_root = Path(__file__).resolve().parents[2]
    candidates.append(repo_root / "dirstral-spec" / SCHEMA_REL)  # submodule
    candidates.append(repo_root.parent / "dirstral-spec" / SCHEMA_REL)  # sibling checkout
    return next((c for c in candidates if c.is_file()), None)


def test_emitted_json_validates_against_design_0004_draft_schema():
    jsonschema = pytest.importorskip("jsonschema")
    schema_path = find_schema()
    if schema_path is None:
        pytest.skip("dirstral-spec design-0004 draft schema not available")
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
    jsonschema.validate(json.loads(emit_json(doc)), schema)
