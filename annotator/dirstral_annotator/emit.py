"""Sidecar emitters.

v0 (`emit_vtt`): the WebVTT convention from dirstral-spec design 0004 §3 —
indexable by dir2mcp today through the existing subtitle-sidecar mechanism
(SPEC §8.6.4) with zero core changes. Cue text carries the human-readable
statement plus a trailing `[sources: …; confidence …]` tail.

v1 (`emit_json`): the draft machine-readable format from design 0004 §4,
validating against the draft schema shipped next to that design note.
"""

from __future__ import annotations

import json
from pathlib import Path

from .model import Document
from .roster import Roster


def vtt_timestamp(seconds: float) -> str:
    if seconds < 0:
        raise ValueError(f"negative timestamp: {seconds}")
    ms = round(seconds * 1000)
    h, rem = divmod(ms, 3_600_000)
    m, rem = divmod(rem, 60_000)
    s, ms = divmod(rem, 1000)
    return f"{h:02d}:{m:02d}:{s:02d}.{ms:03d}"


def cue_text(ann_text: str, sources: tuple[str, ...], confidence: float) -> str:
    tail = f"[sources: {'+'.join(sources)}; confidence {confidence:.2f}]"
    return f"{ann_text} {tail}" if ann_text else tail


def emit_vtt(doc: Document) -> str:
    lines = ["WEBVTT", ""]
    for ann in doc.annotations:
        lines.append(f"{vtt_timestamp(ann.start_s)} --> {vtt_timestamp(ann.end_s)}")
        # VTT cue payloads must not contain blank lines; flatten just in case.
        text = " ".join(cue_text(ann.text, ann.sources, ann.confidence).split("\n"))
        lines.append(text)
        lines.append("")
    return "\n".join(lines)


def emit_json(doc: Document, roster: Roster | None = None) -> str:
    referenced = {e for ann in doc.annotations for e in ann.entity_ids}
    entities = []
    for pid in sorted(referenced):
        player = roster.get(pid) if roster else None
        if player:
            entities.append(
                {"id": player.id, "label": player.name, "aliases": list(player.all_names()[1:])}
            )
        else:
            entities.append({"id": pid, "label": pid.split(":", 1)[-1].replace("-", " ").title()})
    payload = {
        "annotator": {"name": doc.annotator.name, "version": doc.annotator.version},
        "media": doc.media,
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
    return json.dumps(payload, indent=2, ensure_ascii=False) + "\n"


def write_sidecars(doc: Document, media_path: str | Path, roster: Roster | None = None) -> list[Path]:
    """Write the v0 VTT next to the media file (what dir2mcp indexes today)
    and the v1 JSON alongside it (forward-looking; ignored by dir2mcp until
    design 0004 v1 lands). Returns the written paths."""
    media_path = Path(media_path)
    vtt = media_path.with_suffix(".vtt")
    js = media_path.with_name(media_path.stem + ".annotations.json")
    vtt.write_text(emit_vtt(doc), encoding="utf-8")
    js.write_text(emit_json(doc, roster), encoding="utf-8")
    return [vtt, js]
