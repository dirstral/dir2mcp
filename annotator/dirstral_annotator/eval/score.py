"""Score annotator output against ground truth.

The pilot's committed metric is event-level: for each ground-truth pitch,
did some predicted pitch annotation involving that pitcher overlap it
(within tolerance)? Recall = found pitches / all pitches; precision =
predicted pitch annotations that correspond to some real pitch by that
player. Reported overall, per player, and per contributing source (which is
what decides which recognizers the archive actually needs).
"""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, field

from ..model import Annotation
from ..roster import Roster
from .align import Alignment
from .ground_truth import PitchEvent

TOLERANCE_S = 5.0


@dataclass
class PRCount:
    tp: int = 0
    fn: int = 0
    fp: int = 0

    @property
    def recall(self) -> float | None:
        n = self.tp + self.fn
        return self.tp / n if n else None

    @property
    def precision(self) -> float | None:
        n = self.tp + self.fp
        return self.tp / n if n else None


@dataclass
class Scorecard:
    overall: PRCount = field(default_factory=PRCount)
    per_player: dict[str, PRCount] = field(default_factory=lambda: defaultdict(PRCount))
    # source tag -> pitches found by annotations that source contributed to
    per_source_found: dict[str, int] = field(default_factory=lambda: defaultdict(int))
    total_events: int = 0


def score(
    annotations: list[Annotation],
    events: list[PitchEvent],
    alignment: Alignment,
    roster: Roster,
    tolerance_s: float = TOLERANCE_S,
) -> Scorecard:
    card = Scorecard()
    pitch_anns = [a for a in annotations if a.event == "pitch"]
    matched_anns: set[int] = set()

    # Recall: every rostered ground-truth pitch must be covered.
    scored_events = []
    for ev in events:
        pitcher = roster.by_mlbam(ev.pitcher_id)
        if pitcher is None:
            continue  # not on the pilot roster; out of scope for the metric
        scored_events.append((ev, pitcher))
    card.total_events = len(scored_events)

    for ev, pitcher in scored_events:
        t = alignment.to_video(ev.epoch_s)
        found = False
        for i, ann in enumerate(pitch_anns):
            if pitcher.id not in ann.entity_ids:
                continue
            if ann.start_s - tolerance_s <= t <= ann.end_s + tolerance_s:
                found = True
                matched_anns.add(i)
                for src in ann.sources:
                    card.per_source_found[src] += 1
                break
        bucket = card.per_player[pitcher.id]
        if found:
            card.overall.tp += 1
            bucket.tp += 1
        else:
            card.overall.fn += 1
            bucket.fn += 1

    # Precision: every predicted pitch annotation must correspond to a real one.
    for i, ann in enumerate(pitch_anns):
        if i in matched_anns:
            continue
        card.overall.fp += 1
        for pid in ann.entity_ids:
            if roster.get(pid):
                card.per_player[pid].fp += 1
    return card
