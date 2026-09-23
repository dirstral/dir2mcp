"""Why a pitch went uncredited, and why a `pitch` annotation was a false
positive (dir2mcp #1025).

The scorecard says how many; this says which kind, so a recall or precision
loss has a location and a cause instead of a count. Every classification is
read off the scorecard's own greedy matching (`Scorecard.matched`), so the
diagnosis can never disagree with the number it explains.

Uncredited pitch (a recall loss), by the nearest `pitch` annotation naming its
pitcher:

  consumed     an annotation covers the pitch within tolerance but the scorer
               already credited it to another pitch: one cue spanned two
               pitches, and the scorer gives each annotation to at most one.
  late         the nearest annotation starts after the pitch, within NEAR_S:
               the cue arrived too late to claim it.
  early        the nearest annotation ends before the pitch, within NEAR_S.
  misattributed  no annotation of this pitcher is near, but another pitcher's
               `pitch` annotation covers the moment: the bug named the wrong
               field.
  none         nothing within NEAR_S names this pitcher.

False positive (a precision loss), by the nearest real pitch of the named
pitcher:

  taken        a real pitch lies within tolerance but another annotation was
               credited with it (a duplicate the dedupe rule did not catch).
  late / early the nearest real pitch is outside tolerance but within NEAR_S.
  invented     no pitch by that pitcher within NEAR_S.

Annotations are also told apart by KIND: a `count` cue ("Pitch 13 by ...",
from the bug's pitch counter) or a `graphic` cue (the speed graphic), because
the two paths fail differently and are fixed in different places.
"""
from __future__ import annotations

import re
from collections import Counter, defaultdict
from dataclasses import dataclass, field

from ..model import Annotation
from ..roster import Roster
from .align import Alignment
from .score import Scorecard

#: How far the diagnosis looks for the "real pitch this cue meant" before it
#: calls a cue invented, or a pitch uncovered. Wider than the bug is ever off
#: screen between two pitches on the pilot game.
NEAR_S = 30.0
#: Video-time bucket for the per-stretch table, in seconds.
STRETCH_S = 600.0

_COUNT_TEXT = re.compile(r"^Pitch \d+ by ")


def cue_kind(ann: Annotation) -> str:
    return "count" if _COUNT_TEXT.match(ann.text or "") else "graphic"


@dataclass
class Miss:
    """One uncredited pitch."""
    event_index: int
    pitcher_id: str
    t: float
    cause: str
    distance_s: float | None  # signed: annotation minus pitch, None for `none`
    kind: str | None          # kind of the annotation the cause refers to


@dataclass
class FalsePositive:
    ann_index: int
    pitcher_id: str
    start_s: float
    end_s: float
    kind: str
    cause: str
    distance_s: float | None  # signed: annotation minus pitch, None for `invented`


@dataclass
class MissReport:
    tolerance_s: float
    near_s: float = NEAR_S
    misses: list[Miss] = field(default_factory=list)
    false_positives: list[FalsePositive] = field(default_factory=list)

    def miss_causes(self) -> Counter:
        return Counter(m.cause if m.kind is None else f"{m.cause}/{m.kind}" for m in self.misses)

    def fp_causes(self) -> Counter:
        return Counter(f"{f.cause}/{f.kind}" for f in self.false_positives)

    def per_pitcher(self) -> dict[str, tuple[Counter, Counter]]:
        out: dict[str, tuple[Counter, Counter]] = defaultdict(lambda: (Counter(), Counter()))
        for m in self.misses:
            out[m.pitcher_id][0][m.cause] += 1
        for f in self.false_positives:
            out[f.pitcher_id][1][f.cause] += 1
        return dict(out)


def _signed_distance(t: float, ann: Annotation) -> float:
    """Signed seconds from the pitch to the annotation's window: 0 inside,
    positive when the window starts after the pitch, negative when it ended
    before."""
    if ann.start_s <= t <= ann.end_s:
        return 0.0
    return ann.start_s - t if ann.start_s > t else ann.end_s - t


def diagnose(card: Scorecard, alignment: Alignment, roster: Roster,
             tolerance_s: float | None = None, near_s: float = NEAR_S) -> MissReport:
    # Default to the tolerance the scorecard was matched with: a different one
    # would label an unmatched pair "consumed" or "taken".
    if tolerance_s is None:
        tolerance_s = card.tolerance_s
    rep = MissReport(tolerance_s=tolerance_s, near_s=near_s)
    anns = card.pitch_anns
    matched_anns = set(card.matched.values())
    pitch_times = [(alignment.to_video(ev.epoch_s), pitcher.id, k)
                   for k, (ev, pitcher) in enumerate(card.scored)]

    for k, (ev, pitcher) in enumerate(card.scored):
        if k in card.matched:
            continue
        t = alignment.to_video(ev.epoch_s)
        own = [(abs(_signed_distance(t, a)), _signed_distance(t, a), i, a)
               for i, a in enumerate(anns) if pitcher.id in a.entity_ids]
        own = [o for o in own if o[0] <= near_s]
        own.sort(key=lambda o: o[0])
        if own:
            dist, signed, i, a = own[0]
            if dist <= tolerance_s:
                # Inside tolerance yet uncredited: the scorer must have given
                # that annotation to another pitch, or it would have matched.
                cause = "consumed"
            else:
                cause = "late" if signed > 0 else "early"
            rep.misses.append(Miss(k, pitcher.id, t, cause, round(signed, 1), cue_kind(a)))
            continue
        other = [a for a in anns if a.start_s - tolerance_s <= t <= a.end_s + tolerance_s]
        if other:
            rep.misses.append(Miss(k, pitcher.id, t, "misattributed", 0.0, cue_kind(other[0])))
        else:
            rep.misses.append(Miss(k, pitcher.id, t, "none", None, None))

    for i, a in enumerate(anns):
        if i in matched_anns:
            continue
        pids = [pid for pid in a.entity_ids if roster.get(pid)]
        pid = pids[0] if pids else (a.entity_ids[0] if a.entity_ids else "?")
        kind = cue_kind(a)
        near = sorted(((abs(_signed_distance(t, a)), _signed_distance(t, a), k)
                       for t, p, k in pitch_times if p == pid), key=lambda o: o[0])
        near = [n for n in near if n[0] <= near_s]
        if not near:
            rep.false_positives.append(FalsePositive(i, pid, a.start_s, a.end_s, kind, "invented", None))
            continue
        dist, signed, k = near[0]
        if dist <= tolerance_s:
            cause = "taken"
        else:
            cause = "late" if signed > 0 else "early"
        rep.false_positives.append(FalsePositive(i, pid, a.start_s, a.end_s, kind, cause, round(signed, 1)))
    return rep


def stretches(card: Scorecard, alignment: Alignment, rep: MissReport,
              stretch_s: float = STRETCH_S) -> list[tuple[int, int, int, int, str]]:
    """Per video-time bucket: (bucket_start_s, tp, fn, fp, dominant miss cause)."""
    buckets: dict[int, list] = defaultdict(lambda: [0, 0, 0, Counter()])
    for k, (ev, _pitcher) in enumerate(card.scored):
        b = int(alignment.to_video(ev.epoch_s) // stretch_s)
        if k in card.matched:
            buckets[b][0] += 1
        else:
            buckets[b][1] += 1
    for m in rep.misses:
        buckets[int(m.t // stretch_s)][3][m.cause] += 1
    for f in rep.false_positives:
        buckets[int(((f.start_s + f.end_s) / 2.0) // stretch_s)][2] += 1
    out = []
    for b in sorted(buckets):
        tp, fn, fp, causes = buckets[b]
        dominant = causes.most_common(1)[0][0] if causes else "-"
        out.append((int(b * stretch_s), tp, fn, fp, dominant))
    return out


def render(card: Scorecard, alignment: Alignment, roster: Roster, rep: MissReport) -> list[str]:
    lines = [
        "## Why pitches were missed, and what the false positives were",
        "",
        f"Read off the scorecard's own matching (tolerance ±{rep.tolerance_s:.1f}s, "
        f"nearest-pitch search ±{rep.near_s:.0f}s). `count` is a cue from the bug's pitch "
        "counter, `graphic` one from the speed graphic.",
        "",
        "| Uncredited pitches, by cause/kind | n |",
        "|---|---|",
    ]
    for cause, n in rep.miss_causes().most_common():
        lines.append(f"| {cause} | {n} |")
    if not rep.misses:
        lines.append("| _none_ | 0 |")
    lines += ["", "| False positives, by cause/kind | n |", "|---|---|"]
    for cause, n in rep.fp_causes().most_common():
        lines.append(f"| {cause} | {n} |")
    if not rep.false_positives:
        lines.append("| _none_ | 0 |")
    lines += ["", "### Per pitcher", "", "| Pitcher | misses | false positives |", "|---|---|---|"]
    for pid, (mc, fc) in sorted(rep.per_pitcher().items()):
        player = roster.get(pid)
        name = player.name if player else pid
        lines.append(f"| {name} | {_fmt(mc)} | {_fmt(fc)} |")
    lines += ["", f"### Per stretch of video ({int(STRETCH_S // 60)} min buckets)", "",
              "| from | TP | FN | FP | dominant miss cause |", "|---|---|---|---|---|"]
    for start, tp, fn, fp, dominant in stretches(card, alignment, rep):
        lines.append(f"| {int(start // 60):d} min | {tp} | {fn} | {fp} | {dominant} |")
    lines.append("")
    return lines


def _fmt(c: Counter) -> str:
    return ", ".join(f"{k} {v}" for k, v in c.most_common()) or "-"
