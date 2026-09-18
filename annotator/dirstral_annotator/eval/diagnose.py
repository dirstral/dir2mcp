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

import hashlib
import json

import statistics
from bisect import bisect_left, bisect_right
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from ..fusion import fuse
from ..model import Annotation, Cue
from ..roster import Roster
from ..recognizers.base import media_identity
from .align import Alignment
from .ground_truth import PitchEvent
from .score import SCORED_EVENT, TOLERANCE_S, Scorecard

if TYPE_CHECKING:  # pragma: no cover - import cycle guard, typing only
    from ..pipeline import Pipeline

# How many example cues a --debug listing shows per source.
DEBUG_SAMPLE = 10


#: Bumped when the on-disk cue cache stops being readable by this code.
CUE_CACHE_SCHEMA = 1

#: The one source a `--vision-only` run subtracts. See `cache_differences`.
PLAYBYPLAY_SOURCE = "playbyplay"


class CueCacheMismatch(Exception):
    """A cached cascade does not describe the run that asked to replay it."""


class IncompleteCascade(Exception):
    """A run asked to record cues after part of the cascade did not run."""


def roster_digest(roster) -> str:
    """A stable digest of the entity vocabulary a run resolved against.

    Cues are ALREADY resolved: they carry entity ids and text built from a
    player's display name. So a cue file recorded against one roster replayed
    against another names the wrong people, and the scorer resolves ground
    truth through `mlbam_id`, which the roster also owns. Every field that can
    move an id or a name is therefore in the digest.

    The OCR read log needs no equivalent, though not as cleanly. It records
    text and re-runs interpretation on replay, so a changed roster reaches the
    cues. What it cannot re-run is the band search: `ScorebugRecognizer`
    counts a roster match as a hit, and the hit count is what steers
    `_RegionSearch` and `_AdaptiveFallback`. A replay therefore reads the
    bands the RECORDED roster settled on. That is close enough to be useful
    and not the same as a fresh pass, which is why this digest guards the cue
    file and `OverlayReader._replay` states the limit rather than hiding it.
    """
    rows = sorted(
        [p.id, p.name, p.number or "", *sorted(p.aliases)] for p in roster.players
    )
    mlbam = sorted((str(k), v) for k, v in roster.mlbam_ids.items())
    payload = json.dumps([rows, mlbam], sort_keys=True).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()[:16]


def cascade_fingerprint(pipeline: "Pipeline", media_path: Path) -> dict:
    """The settings that decide WHICH cues the cascade produces.

    `min_confidence` is deliberately absent. It is a fusion-time floor, not a
    cascade input, so a cached run is free to sweep it. Every field below does
    change the cues, so replaying a cache under a different value would report
    a number for a configuration that never ran.
    """
    return {
        "media": media_identity(media_path),
        "roster": roster_digest(pipeline.roster),
        "playbyplay": [
            {
                "media": name,
                "game_pk": game.game_pk,
                "feed": game.feed,
                # Anchors map wall clock onto this video, so they move every
                # play-by-play cue: a re-anchored run is a different cue set.
                "anchors": [[a.epoch_s, a.video_s] for a in game.anchors],
            }
            for name, game in sorted(pipeline.games.items())
        ],
        "scorebug": pipeline.scorebug,
        "scorebug_pitch_counts": pipeline.scorebug_pitch_counts,
        "jersey": pipeline.jersey,
        "news": pipeline.news,
        "news_min_chars": pipeline.news_min_chars,
        "news_min_agreement": pipeline.news_min_agreement,
        "ocr_lang": pipeline.ocr_lang,
        "faces_bank": str(pipeline.faces_bank) if pipeline.faces_bank else None,
        # A loaded backend is a callable and cannot be fingerprinted, so its
        # presence is recorded here and what it was BUILT from is recorded by
        # the builder (`Pipeline.caption_config`). That covers the settings an
        # operator changes, `--caption-model` and `--caption-prompt` among
        # them. It does not cover a model whose weights changed under a
        # stable id; nothing short of hashing them would.
        "caption": pipeline.caption_fn is not None,
        "caption_probe": pipeline.probe_fn is not None,
        "caption_config": dict(sorted(pipeline.caption_config.items())),
        "caption_fps": pipeline.caption_fps,
        "caption_prefix": pipeline.caption_prefix,
        "caption_windows": (
            [list(w) for w in pipeline.caption_windows]
            if pipeline.caption_windows is not None else None
        ),
        "caption_floor_fps": pipeline.caption_floor_fps,
        "caption_max_span": pipeline.caption_max_span,
        "caption_drop_uninformative": pipeline.caption_drop_uninformative,
        "fps": pipeline.fps,
    }


def cache_differences(cached: dict, current: dict) -> list[str]:
    """Report the fields that make `cached` unusable for a `current` run.

    Empty means the cache may be replayed. The point of the check is that a
    silent replay under changed settings produces a scorecard for a
    configuration that never ran, which is worse than the hours the cache
    saves.

    `playbyplay` is exempt in exactly one direction. A cache recorded WITH a
    feed binding can serve a `--vision-only` run, because vision-only is that
    same cue set minus the play-by-play source, and the caller subtracts it.
    The reverse cannot be served: those cues are not in the file.
    """
    diffs = []
    for key in sorted(set(cached) | set(current)):
        if key == "playbyplay":
            continue
        if cached.get(key) != current.get(key):
            diffs.append(f"{key}: cached={cached.get(key)!r}, this run={current.get(key)!r}")
    if current.get("playbyplay") and cached.get("playbyplay") != current.get("playbyplay"):
        diffs.append(
            f"playbyplay: cached={cached.get('playbyplay')!r}, "
            f"this run={current.get('playbyplay')!r}"
        )
    return diffs


def cues_to_json(cues: "list[Cue]", meta: dict) -> str:
    """Serialise pre-fusion cues so a later run can skip the cascade.

    The cascade is the only expensive stage: OCR and face embedding over a
    3h24m proxy take hours, while fusion, scoring and diagnostics take
    seconds. Without a cache, testing one fusion or scoring change costs a
    full re-read of the video, which is why the pitch-count trade (#741) sat
    unexamined between two report files.
    """
    payload = {
        "schema": CUE_CACHE_SCHEMA,
        "cascade": meta,
        "cues": [
            {
                "source": c.source,
                "start_s": c.start_s,
                "end_s": c.end_s,
                "event": c.event,
                "entity_ids": list(c.entity_ids),
                "confidence": c.confidence,
                "text": c.text,
                "attributes": dict(c.attributes),
            }
            for c in cues
        ],
    }
    return json.dumps(payload, indent=1, sort_keys=True)


def cues_from_json(raw: str) -> tuple[dict, list[Cue]]:
    """Rebuild the cascade settings and cues written by `cues_to_json`.

    `entity_ids` is restored as a tuple because `Cue` is frozen and compares
    by value; a list would make a round-tripped cue unequal to the one the
    cascade produced.
    """
    # A hand-edited, truncated or foreign file is a stale cache, not a crash.
    # Every shape error below becomes the same exception, because the CLI
    # turns exactly one type into a readable message and a `KeyError` from
    # here would surface as a traceback instead.
    try:
        payload = json.loads(raw)
    except ValueError as exc:
        raise CueCacheMismatch(f"cue cache is not readable JSON ({exc})") from exc
    if not isinstance(payload, dict):
        raise CueCacheMismatch(
            f"cue cache holds a {type(payload).__name__} where an object belongs"
        )
    schema = payload.get("schema")
    # `type(...) is not int` as well as the comparison: `True == 1` in Python,
    # so a JSON `true` would otherwise read as schema 1.
    if type(schema) is not int or schema != CUE_CACHE_SCHEMA:
        raise CueCacheMismatch(
            f"cue cache schema {schema!r}, this build reads {CUE_CACHE_SCHEMA}; "
            "re-run the cascade to rewrite it"
        )
    cascade = payload.get("cascade")
    if not isinstance(cascade, dict):
        raise CueCacheMismatch("cue cache records no cascade settings")
    records = payload.get("cues")
    if not isinstance(records, list):
        raise CueCacheMismatch("cue cache holds no cue list")
    cues = []
    for i, d in enumerate(records):
        try:
            cues.append(Cue(
                source=str(d["source"]),
                start_s=float(d["start_s"]),
                end_s=float(d["end_s"]),
                event=str(d["event"]),
                entity_ids=tuple(str(e) for e in (d.get("entity_ids") or ())),
                confidence=float(d["confidence"]),
                text=str(d.get("text", "")),
                attributes={str(k): str(v) for k, v in (d.get("attributes") or {}).items()},
            ))
        except (ValueError, TypeError, KeyError, AttributeError) as exc:
            raise CueCacheMismatch(
                f"cue cache record {i} is not a readable cue "
                f"({type(exc).__name__}: {exc})"
            ) from exc
    return cascade, cues


def run_pipeline(
    pipeline: "Pipeline",
    media_path: Path,
    *,
    cues_in: "Path | None" = None,
    cues_out: "Path | None" = None,
) -> tuple[list[Cue], list[Annotation]]:
    """Run the cascade keeping the pre-fusion cues.

    `Pipeline.annotations_for` throws the cues away, and the cues are what
    make a zero row explainable (how many a source emitted before fusion and
    before the confidence floor).

    `cues_in` replays a recorded cascade instead of running it, and refuses a
    cache whose settings differ (`cache_differences`). Fusion, the confidence
    floor and scoring still run, so a replay answers questions about those.
    It says nothing about a change to a recognizer: that needs the cascade.
    """
    fingerprint = cascade_fingerprint(pipeline, media_path)
    if cues_in is not None:
        # The read itself, not just the parse: a path that is missing, is a
        # directory, or cannot be opened is the same kind of problem as a
        # malformed one, and the CLI turns exactly one exception type into a
        # message rather than a traceback.
        try:
            raw = cues_in.read_text(encoding="utf-8")
        except OSError as exc:
            raise CueCacheMismatch(f"cannot read {cues_in}: {exc}") from exc
        cached, cues = cues_from_json(raw)
        diffs = cache_differences(cached, fingerprint)
        if diffs:
            raise CueCacheMismatch(
                f"{cues_in} was recorded for a different cascade:\n  "
                + "\n  ".join(diffs)
                + "\nre-run without --cues to rebuild it"
            )
        if not fingerprint["playbyplay"] and cached.get("playbyplay"):
            cues = [c for c in cues if c.source != PLAYBYPLAY_SOURCE]
    else:
        cues = pipeline.cues_for(media_path)
    if cues_out is not None:
        # Same rule as the read log one layer down: publish a complete pass or
        # nothing. A recognizer that was skipped (no engine installed, a
        # backend that failed) leaves its cues out, and a file recorded then
        # replays as the full cascade. The caller reports `pipeline.skipped`
        # either way, so the reason is never lost.
        if pipeline.skipped:
            raise IncompleteCascade(
                f"{len(pipeline.skipped)} recognizer(s) were skipped, so this run is "
                "not a cascade worth recording:\n  " + "\n  ".join(pipeline.skipped)
            )
        cues_out.write_text(cues_to_json(cues, fingerprint), encoding="utf-8")
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
