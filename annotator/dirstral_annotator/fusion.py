"""Fuse per-recognizer cues into emit-ready annotations.

Cues that make the same claim — same event type, overlapping (or nearly
overlapping) time ranges, at least one shared entity — merge into one
annotation whose time range is the union, whose sources accumulate, and
whose confidence combines under independence (noisy-OR): two weak agreeing
signals beat either alone, which is the entire point of the cascade.
"""

from __future__ import annotations

from .model import Annotation, Cue
from .recognizers.base import TEXT_RUN_SIMILARITY, text_overlap

# Cues for the same claim can disagree slightly on boundaries (scorebug
# appears a beat after the play-by-play timestamp); ranges closer than this
# gap (seconds) still merge.
MERGE_GAP_S = 2.0

# How much two entity-free cues' text must overlap to count as the same claim.
# The same threshold the run collapsing uses, because it is the same question:
# these are two views of one passage, or they are two passages.
TEXT_MERGE_SIMILARITY = TEXT_RUN_SIMILARITY


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
    if not _same_claim_by_text(group, cue, entities):
        return False
    start = min(g.start_s for g in group)
    end = max(g.end_s for g in group)
    return cue.start_s <= end + MERGE_GAP_S and cue.end_s >= start - MERGE_GAP_S


def _same_claim_by_text(group: list[Cue], cue: Cue, entities: set[str]) -> bool:
    """For cues nothing identifies but their words, whether the words agree.

    The entity test above only REJECTS when both sides name entities and the
    names disagree, so a pair of cues that name nobody skips it entirely and
    falls through to the time test. That is right for the baseball cascade,
    where every cue resolves a roster, and wrong for overlay text, which
    resolves nothing: consecutive ticker passages share an event, carry no
    entities and sit next to each other in time, so they all merged into one
    annotation. On 90s of TV Rain that turned nine ticker cues into four, one
    of which spanned 58 seconds and five separate stories while `_merge` kept
    only the longest text and dropped the other four outright.

    So when neither side names anyone and both carry text, the text decides.
    Two ticker cues describing different stories are different claims, however
    close together they ran. The same threshold as the run collapsing that
    produced them, because it is the same question: is this the same passage?

    Cues with no text on either side are unaffected, and so is every existing
    caller: the baseball recognizers all resolve entities.
    """
    if cue.entity_ids or entities or not cue.text.strip():
        return True
    texts = [g.text for g in group if g.text.strip()]
    if not texts:
        return True
    return any(text_overlap(text, cue.text) >= TEXT_MERGE_SIMILARITY for text in texts)


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
