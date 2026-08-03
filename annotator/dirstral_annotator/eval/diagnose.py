"""Per-source diagnostics: why a recognizer contributed nothing.

`score()` answers exactly one question: for each ground-truth pitch, did some
predicted annotation with event `pitch`, naming that *pitcher*, overlap it?
Both qualifiers are load-bearing. A recognizer that never emits a `pitch` cue,
or never names the pitcher, cannot appear in the "found by source" table
however well it works: its zero is a property of the metric, not a measurement
of the recognizer.

Reading that table on its own therefore invites the wrong conclusion. These
diagnostics separate the cases that all render as an absent row:

  * structurally ineligible: the source's cues are not the kind of claim the
    metric scores (wrong event, or never names the pitcher), so no amount of
    recognizer quality could ever produce a hit;
  * dropped: cues existed and fused, but the `--min-confidence` floor removed
    the annotation;
  * genuinely weak: eligible cues existed and simply did not land near a
    ground-truth pitch. The near-miss distances say how far off they were.

Nothing here changes the committed accuracy metric. It reports the same run
from the recognizers' side so "vision is weak" and "vision is invisible to
this scorer" stop looking identical.
"""

from __future__ import annotations

import statistics
from bisect import bisect_left, bisect_right
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from ..fusion import fuse
from ..model import Annotation, Cue
from ..roster import Roster
from .align import Alignment
from .ground_truth import PitchEvent
from .score import SCORED_EVENT, TOLERANCE_S, Scorecard

if TYPE_CHECKING:  # pragma: no cover - import cycle guard, typing only
    from ..pipeline import Pipeline

# How many example cues a --debug listing shows per source.
DEBUG_SAMPLE = 10


def run_pipeline(pipeline: "Pipeline", media_path: Path) -> tuple[list[Cue], list[Annotation]]:
    """Run the cascade keeping the pre-fusion cues.

    `Pipeline.annotations_for` throws the cues away, and the cues are what
    make a zero row explainable (how many a source emitted before fusion and
    before the confidence floor).
    """
    cues = pipeline.cues_for(media_path)
    return cues, fuse(cues, min_confidence=pipeline.min_confidence)


@dataclass(frozen=True)
class ScoredWindow:
    """One ground-truth pitch as the scorer sees it: on the video timeline,
    with the roster ids of the two players the play involves."""

    video_s: float
    pitcher_id: str
    batter_id: str | None


@dataclass
class SourceDiagnostics:
    source: str
    cues: int = 0
    cues_by_event: dict[str, int] = field(default_factory=lambda: defaultdict(int))
    annotations_fused: int = 0  # fused annotations this source contributed to
    annotations_dropped: int = 0  # ... that the min-confidence floor removed
    scored_event_cues: int = 0  # cues whose event the scorer even looks at
    cues_near_pitch: int = 0  # cues overlapping a scored pitch within tolerance
    cues_naming_pitcher: int = 0  # ... that name that pitch's pitcher
    cues_naming_batter: int = 0  # ... that name that pitch's batter
    unknown_entity_ids: set[str] = field(default_factory=set)
    # Where on the video timeline this source's cues actually sit. A span that
    # runs past the media is a plumbing fault (corrupt frame timestamps), not a
    # recognition result, and it is invisible in any per-pitch count.
    first_cue_s: float | None = None
    last_cue_s: float | None = None
    near_miss_s: list[float] = field(default_factory=list)  # off-target cue distances
    pitches_with_pitcher_cue: int = 0
    pitches_with_batter_cue: int = 0
    pitches_covered: int = 0  # pitcher or batter named within tolerance
    #: Pitches whose pitcher or batter this source names SOMEWHERE in the media,
    #: at any time. It is the source's own effective vocabulary, so it separates
    #: "cannot identify this person at all" from "did not identify them here".
    pitches_reachable: int = 0
    found_pitches: int = 0  # credited by the scorecard
    samples: list[Cue] = field(default_factory=list)

    @property
    def verdict(self) -> str:
        if self.found_pitches:
            return f"counted: credited on {self.found_pitches} scored pitch(es)"
        if not self.cues:
            return "no cues emitted (recognizer produced nothing, or was skipped)"
        if self.annotations_fused == 0 and self.annotations_dropped:
            return (
                f"dropped: all {self.annotations_dropped} fused annotation(s) fell "
                "below --min-confidence"
            )
        if self.scored_event_cues == 0:
            events = ", ".join(f"`{e}`" for e in sorted(self.cues_by_event))
            return (
                f"UNREACHABLE by this metric: emits {events} cues, never "
                f"`{SCORED_EVENT}`; the scorecard reads `{SCORED_EVENT}` "
                "annotations only"
            )
        # Order matters: "never lands near a pitch" is a recognizer result and
        # must not be dressed up as an unreachable target. Only cues that DO
        # overlap a pitch can testify about the role (pitcher vs batter) they
        # name, so the role verdict comes second.
        if self.cues_near_pitch == 0:
            return "weak: eligible cues exist but none land within tolerance of a pitch"
        if self.cues_naming_pitcher == 0:
            return (
                "UNREACHABLE by this metric: never names the ground-truth "
                "pitcher of a pitch it overlaps (the metric is pitcher-keyed)"
            )
        return "eligible and on time but uncredited: scorer plumbing needs a look"

    @property
    def unreachable(self) -> bool:
        return self.verdict.startswith("UNREACHABLE")


@dataclass
class Diagnostics:
    tolerance_s: float
    min_confidence: float
    scored_pitches: int
    per_source: dict[str, SourceDiagnostics] = field(default_factory=dict)

    @property
    def unreachable_sources(self) -> list[str]:
        return sorted(s for s, d in self.per_source.items() if d.unreachable)


def scored_windows(
    events: list[PitchEvent], alignment: Alignment, roster: Roster
) -> list[ScoredWindow]:
    """The ground-truth pitches `score()` actually scores, on video time.

    Mirrors the scorer's filter exactly (pitches by a rostered pitcher); a
    diagnostic measured against a different denominator would not explain the
    scorecard it sits next to.
    """
    windows = []
    for ev in events:
        pitcher = roster.by_mlbam(ev.pitcher_id)
        if pitcher is None:
            continue
        batter = roster.by_mlbam(ev.batter_id)
        windows.append(
            ScoredWindow(
                video_s=alignment.to_video(ev.epoch_s),
                pitcher_id=pitcher.id,
                batter_id=batter.id if batter else None,
            )
        )
    windows.sort(key=lambda w: w.video_s)
    return windows


def diagnose(
    cues: list[Cue],
    annotations: list[Annotation],
    events: list[PitchEvent],
    alignment: Alignment,
    roster: Roster,
    card: Scorecard,
    min_confidence: float = 0.0,
    tolerance_s: float = TOLERANCE_S,
) -> Diagnostics:
    """Explain, per source, what happened to its cues.

    `annotations` must be the post-floor list the scorecard was built from;
    the unfloored fusion is recomputed here so "dropped by the floor" is
    measured rather than guessed.
    """
    windows = scored_windows(events, alignment, roster)
    times = [w.video_s for w in windows]
    diag = Diagnostics(
        tolerance_s=tolerance_s,
        min_confidence=min_confidence,
        scored_pitches=len(windows),
    )

    def bucket(source: str) -> SourceDiagnostics:
        return diag.per_source.setdefault(source, SourceDiagnostics(source=source))

    # Sources are enumerated from cues *and* annotations: a source whose every
    # annotation was floored still deserves a row, and so does one that only
    # shows up post-fusion.
    for cue in cues:
        bucket(cue.source)
    for ann in annotations:
        for src in ann.sources:
            bucket(src)
    for src, found in card.per_source_found.items():
        bucket(src).found_pitches = found

    # Re-fuse without the floor: the difference against `min_confidence` is
    # what the floor removed, measured rather than guessed. Same cues in, so
    # the groups are identical and only the filter differs.
    unfloored = fuse(cues, min_confidence=0.0) if cues else list(annotations)
    for ann in unfloored:
        dropped = ann.confidence < min_confidence
        for src in ann.sources:
            b = bucket(src)
            if dropped:
                b.annotations_dropped += 1
            else:
                b.annotations_fused += 1

    # Pitch coverage is per-window so one chatty cue cannot cover a pitch twice.
    covered_pitcher: dict[str, set[int]] = defaultdict(set)
    covered_batter: dict[str, set[int]] = defaultdict(set)
    # Every identity a source emits anywhere, used below to compute what it
    # could have covered. A face bank missing half a roster and a recognizer
    # that rarely resolves a face both show up as low raw coverage; only this
    # tells them apart, and they call for opposite responses.
    vocabulary: dict[str, set[str]] = defaultdict(set)

    for cue in cues:
        b = bucket(cue.source)
        b.cues += 1
        b.cues_by_event[cue.event] += 1
        b.first_cue_s = (cue.start_s if b.first_cue_s is None
                         else min(b.first_cue_s, cue.start_s))
        b.last_cue_s = (cue.end_s if b.last_cue_s is None
                        else max(b.last_cue_s, cue.end_s))
        if cue.event == SCORED_EVENT:
            b.scored_event_cues += 1
        for pid in cue.entity_ids:
            vocabulary[cue.source].add(pid)
            if roster.get(pid) is None:
                b.unknown_entity_ids.add(pid)
        if len(b.samples) < DEBUG_SAMPLE:
            b.samples.append(cue)

        lo = bisect_left(times, cue.start_s - tolerance_s)
        hi = bisect_right(times, cue.end_s + tolerance_s)
        if lo >= hi:
            b.near_miss_s.append(_distance_to_nearest(cue, times, lo))
            continue
        b.cues_near_pitch += 1
        named_pitcher = named_batter = False
        for idx in range(lo, hi):
            w = windows[idx]
            if w.pitcher_id in cue.entity_ids:
                named_pitcher = True
                covered_pitcher[cue.source].add(idx)
            if w.batter_id is not None and w.batter_id in cue.entity_ids:
                named_batter = True
                covered_batter[cue.source].add(idx)
        b.cues_naming_pitcher += int(named_pitcher)
        b.cues_naming_batter += int(named_batter)

    for src, b in diag.per_source.items():
        b.pitches_with_pitcher_cue = len(covered_pitcher[src])
        b.pitches_with_batter_cue = len(covered_batter[src])
        b.pitches_covered = len(covered_pitcher[src] | covered_batter[src])
        known = vocabulary[src]
        b.pitches_reachable = sum(
            1 for w in windows
            if w.pitcher_id in known or (w.batter_id is not None and w.batter_id in known)
        )
    return diag


def near_miss_summary(distances: list[float]) -> dict[str, float] | None:
    """min / median / p90 / max of off-target cue distances, or None."""
    if not distances:
        return None
    ordered = sorted(distances)
    p90 = ordered[min(len(ordered) - 1, int(round(0.9 * (len(ordered) - 1))))]
    return {
        "count": float(len(ordered)),
        "min": ordered[0],
        "median": statistics.median(ordered),
        "p90": p90,
        "max": ordered[-1],
    }


def _distance_to_nearest(cue: Cue, times: list[float], lo: int) -> float:
    """Seconds between this cue's range and the closest scored pitch.

    Zero would mean overlap, so callers only reach this for cues that already
    failed the tolerance test; `inf` when there is nothing to be near.
    """
    best = float("inf")
    for idx in (lo - 1, lo):
        if 0 <= idx < len(times):
            t = times[idx]
            best = min(best, max(cue.start_s - t, t - cue.end_s, 0.0))
    return best
