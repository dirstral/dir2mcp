"""Wall-clock → video-time alignment.

An anchor is a human (or scorebug-OCR-bootstrapped) observation that pitch
event E happens at video time T. One anchor determines the offset; a few
determine it robustly (median) and expose drift — archival transfers and
mid-game stream discontinuities show up as anchor disagreement, which we
surface loudly instead of averaging away.
"""

from __future__ import annotations

import statistics
from dataclasses import dataclass

# Anchors disagreeing by more than this (seconds) mean the video timeline is
# not a single linear shift of wall clock (splice, dropped segment).
DRIFT_WARN_S = 5.0


@dataclass(frozen=True)
class Anchor:
    """`epoch_s`: the event's wall-clock time; `video_s`: where it actually
    appears in this video file."""

    epoch_s: float
    video_s: float


@dataclass(frozen=True)
class Alignment:
    offset_s: float  # video_t = epoch_s + offset_s
    spread_s: float  # max pairwise disagreement between anchors
    anchors: int

    @property
    def drifty(self) -> bool:
        return self.spread_s > DRIFT_WARN_S

    def to_video(self, epoch_s: float) -> float:
        return epoch_s + self.offset_s


def estimate(anchors: list[Anchor]) -> Alignment:
    if not anchors:
        raise ValueError("at least one anchor is required")
    offsets = [a.video_s - a.epoch_s for a in anchors]
    return Alignment(
        offset_s=statistics.median(offsets),
        spread_s=max(offsets) - min(offsets) if len(offsets) > 1 else 0.0,
        anchors=len(anchors),
    )
