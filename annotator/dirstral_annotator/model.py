"""Core data model for the annotator pipeline.

A *cue* is a single recognizer's claim about a time range ("scorebug says
Webb is pitching at 00:42:10-00:42:31"). Cues from all recognizers are fused
into *annotations* — the merged, confidence-scored statements that get
emitted as a sidecar next to the media file (dirstral-spec design 0004).
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Player:
    """One roster entry. `id` is the stable corpus-wide entity id
    (design 0004 `entities[].id`), e.g. "player:webb-logan" or an MLBAM id
    mapped via the roster file."""

    id: str
    name: str
    number: str | None = None
    aliases: tuple[str, ...] = ()

    def all_names(self) -> tuple[str, ...]:
        names = [self.name, *self.aliases]
        if self.number:
            names.append(f"#{self.number}")
        return tuple(names)


@dataclass(frozen=True)
class Cue:
    """A single recognizer observation over a media time range (seconds)."""

    source: str  # "scorebug" | "playbyplay" | "jersey" | "face"
    start_s: float
    end_s: float
    event: str  # "pitch" | "at_bat" | "appearance" | ...
    entity_ids: tuple[str, ...]
    confidence: float
    text: str = ""

    def __post_init__(self) -> None:
        if self.end_s < self.start_s:
            raise ValueError(f"cue ends before it starts: {self}")
        if not 0.0 <= self.confidence <= 1.0:
            raise ValueError(f"confidence out of [0,1]: {self}")


@dataclass
class Annotation:
    """A fused, emit-ready statement (one v1 `annotations[]` element, or one
    VTT cue in the v0 convention)."""

    start_s: float
    end_s: float
    event: str
    entity_ids: tuple[str, ...]
    text: str
    confidence: float
    sources: tuple[str, ...]


@dataclass
class RecognizerInfo:
    """Backend identity declared in every response; feeds the derivation
    identity of the representation dir2mcp persists (design 0004 §4)."""

    name: str = "dirstral-annotator"
    version: str = "0.2.0"


@dataclass
class Document:
    """Everything recognized for one media file."""

    media: str  # media filename this result describes
    recognizer: RecognizerInfo = field(default_factory=RecognizerInfo)
    entities: list[Player] = field(default_factory=list)
    annotations: list[Annotation] = field(default_factory=list)


_SLUG_RE = re.compile(r"[^a-z0-9]+")


def slug_entity_id(name: str) -> str:
    """Fallback entity id for a name with no roster mapping:
    "Logan Webb" -> "player:logan-webb"."""
    return "player:" + _SLUG_RE.sub("-", name.lower()).strip("-")
