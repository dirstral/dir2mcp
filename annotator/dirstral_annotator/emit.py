"""Build the recognize response (design 0004 §5).

This is the wire payload `dirstral-annotator serve` returns from
`POST /recognize` and that dir2mcp's `RecognizeServeClient` consumes. It
validates against the draft schema shipped with dirstral-spec design 0004
(`0004-recognize-response.schema.json`).
"""

from __future__ import annotations

import json

from .model import Document
from .roster import Roster, display_name


def build_response(doc: Document, roster: Roster | None = None) -> dict:
    referenced = {e for ann in doc.annotations for e in ann.entity_ids}
    entities = []
    for pid in sorted(referenced):
        player = roster.get(pid) if roster else None
        if player:
            entities.append(
                {"id": player.id, "label": player.name, "aliases": list(player.all_names()[1:])}
            )
        else:
            entities.append({"id": pid, "label": display_name(roster, pid)})
    return {
        "recognizer": {"name": doc.recognizer.name, "version": doc.recognizer.version},
        "entities": entities,
        "annotations": [
            {
                "start_s": round(a.start_s, 3),
                "end_s": round(a.end_s, 3),
                "event": a.event,
                "entities": list(a.entity_ids),
                "text": a.text,
                "confidence": a.confidence,
                "sources": list(a.sources),
            }
            for a in doc.annotations
        ],
    }


def response_json(doc: Document, roster: Roster | None = None) -> str:
    return json.dumps(build_response(doc, roster), indent=2, ensure_ascii=False) + "\n"
