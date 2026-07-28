"""Fuse per-recognizer cues into emit-ready annotations.

Cues that make the same claim — same event type, overlapping (or nearly
overlapping) time ranges, at least one shared entity — merge into one
annotation whose time range is the union, whose sources accumulate, and
whose confidence combines under independence (noisy-OR): two weak agreeing
signals beat either alone, which is the entire point of the cascade.
"""

from __future__ import annotations

from .model import Annotation, Cue

# Cues for the same claim can disagree slightly on boundaries (scorebug
# appears a beat after the play-by-play timestamp); ranges closer than this
# gap (seconds) still merge.
MERGE_GAP_S = 2.0


def fuse(cues: list[Cue], min_confidence: float = 0.0) -> list[Annotation]:
    """Merge cues into annotations, sorted by start time.

    `min_confidence` drops fused annotations below the floor (the annotator-
    side counterpart of the ingest-side floor proposed in design 0004 §4).
    """
    groups: list[list[Cue]] = []
    for cue in sorted(cues, key=lambda c: (c.start_s, c.end_s)):
        target = None
        for group in groups:
            if _belongs(group, cue):
                target = group
                break
        if target is None:
            groups.append([cue])
        else:
            target.append(cue)

    out = []
    for group in groups:
        ann = _merge(group)
        if ann.confidence >= min_confidence:
            out.append(ann)
    out.sort(key=lambda a: (a.start_s, a.end_s))
    return out


def _belongs(group: list[Cue], cue: Cue) -> bool:
    if any(g.event != cue.event for g in group):
        return False
    entities = {e for g in group for e in g.entity_ids}
    if cue.entity_ids and entities and not entities.intersection(cue.entity_ids):
        return False
    start = min(g.start_s for g in group)
    end = max(g.end_s for g in group)
    return cue.start_s <= end + MERGE_GAP_S and cue.end_s >= start - MERGE_GAP_S


def _merge(group: list[Cue]) -> Annotation:
    # Noisy-OR over independent sources; repeated cues from one source only
    # count once (their max), so a chatty recognizer can't inflate itself.
    best_per_source: dict[str, float] = {}
    for c in group:
        best_per_source[c.source] = max(best_per_source.get(c.source, 0.0), c.confidence)
    miss = 1.0
    for p in best_per_source.values():
        miss *= 1.0 - p
    confidence = round(1.0 - miss, 4)

    entity_ids = tuple(dict.fromkeys(e for c in group for e in c.entity_ids))
    # Prefer the richest text a recognizer produced (play-by-play beats a
    # bare face sighting), falling back to any non-empty one.
    text = max((c.text for c in group), key=len, default="")
    return Annotation(
        start_s=min(c.start_s for c in group),
        end_s=max(c.end_s for c in group),
        event=group[0].event,
        entity_ids=entity_ids,
        text=text,
        confidence=confidence,
        sources=tuple(sorted(best_per_source)),
    )
