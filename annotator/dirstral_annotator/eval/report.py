"""Render a Scorecard as the phase-1 markdown accuracy report.

The scorecard alone is not a readable report. Its headline pair and its
per-source table are both narrower than they look: with play-by-play enabled
the headline mostly measures wall-clock alignment, and the source table can
only list sources that reached a credited `pitch` annotation. When
`eval.diagnose` output is supplied, the renderer states both limits and prints
the per-source evidence, so an absent source reads as "not eligible" or "weak"
rather than as an unexplained zero.
"""

from __future__ import annotations

from ..roster import Roster
from .align import Alignment
from .diagnose import DEBUG_SAMPLE, Diagnostics, near_miss_summary
from .score import SCORED_EVENT, Scorecard


def _pct(v: float | None) -> str:
    return f"{v * 100:.1f}%" if v is not None else "n/a"


def render(
    card: Scorecard,
    alignment: Alignment,
    roster: Roster,
    title: str,
    diagnostics: Diagnostics | None = None,
    debug: bool = False,
) -> str:
    lines = [
        f"# Accuracy report — {title}",
        "",
        f"- Ground-truth pitches scored (rostered pitchers): **{card.total_events}**",
        f"- Overall recall: **{_pct(card.overall.recall)}** "
        f"(target ≥ 90%) · precision: **{_pct(card.overall.precision)}** (target ≥ 95%)",
        f"- Alignment: offset {alignment.offset_s:+.2f}s from {alignment.anchors} anchor(s), "
        f"spread {alignment.spread_s:.2f}s"
        + (" — **DRIFT WARNING: timeline is not a single linear shift; "
           "re-anchor per segment**" if alignment.drifty else ""),
        "",
        f"> Scope of the headline: it scores `{SCORED_EVENT}` annotations naming the "
        "ground-truth **pitcher**. When play-by-play is enabled it is therefore "
        "mostly a measurement of wall-clock alignment, not of computer vision.",
        "",
        "## Per player",
        "",
        "| Player | Recall | Precision | TP | FN | FP |",
        "|---|---|---|---|---|---|",
    ]
    for pid in sorted(card.per_player):
        pr = card.per_player[pid]
        player = roster.get(pid)
        name = player.name if player else pid
        lines.append(
            f"| {name} | {_pct(pr.recall)} | {_pct(pr.precision)} "
            f"| {pr.tp} | {pr.fn} | {pr.fp} |"
        )
    lines += [
        "",
        "## Pitches found, by contributing source",
        "",
        f"Counts sources that reached a credited `{SCORED_EVENT}` annotation naming "
        "the pitcher. **An absent source has not been measured as weak**; it may "
        "be making a kind of claim this metric never reads. See the diagnostics "
        "below before drawing any conclusion from this table.",
        "",
        "| Source | Found pitches it contributed to |",
        "|---|---|",
    ]
    for src in sorted(card.per_source_found):
        lines.append(f"| {src} | {card.per_source_found[src]} |")
    if not card.per_source_found:
        lines.append("| _none_ | 0 |")
    lines.append("")
    if diagnostics is not None:
        lines += _diagnostics_sections(diagnostics, roster, debug)
    return "\n".join(lines)


def _diagnostics_sections(diag: Diagnostics, roster: Roster, debug: bool) -> list[str]:
    lines = [
        "## Source diagnostics",
        "",
        f"Tolerance ±{diag.tolerance_s:.1f}s; confidence floor "
        f"{diag.min_confidence:.2f}; {diag.scored_pitches} scored pitch(es).",
        "",
        "| Source | Cues | Events | Cue span (s) | Fused anns | Floored "
        "| Scored-event cues | Cues near a pitch | Verdict |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for src in sorted(diag.per_source):
        d = diag.per_source[src]
        events = ", ".join(f"{e} {n}" for e, n in sorted(d.cues_by_event.items())) or "none"
        span = ("none" if d.first_cue_s is None
                else f"{d.first_cue_s:.0f}..{d.last_cue_s:.0f}")
        lines.append(
            f"| {src} | {d.cues} | {events} | {span} | {d.annotations_fused} "
            f"| {d.annotations_dropped} | {d.scored_event_cues} "
            f"| {d.cues_near_pitch} | {d.verdict} |"
        )
    lines += [
        "",
        "Read `Cue span` against the media's own duration first: a source whose "
        "cues run past the end of the file has corrupt timestamps, and every "
        "per-pitch number for it below is meaningless until that is fixed.",
    ]
    unreachable = diag.unreachable_sources
    if unreachable:
        lines += [
            "",
            f"> **{', '.join(unreachable)} cannot score on this metric.** The metric "
            f"asks a pitcher-keyed question about `{SCORED_EVENT}` annotations; these "
            "sources report who is visible on screen, which at a pitch is usually the "
            "batter. Their absence from the table above is a property of the target, "
            "not a recognizer result. Judge them on the identity-coverage numbers "
            "below, and change the metric (not the recognizers) if pitcher-keyed "
            f"`{SCORED_EVENT}` coverage is what the archive needs from them.",
        ]

    lines += [
        "",
        "### Identity coverage (diagnostic, NOT the phase-1 gate)",
        "",
        f"Role-agnostic: of the {diag.scored_pitches} scored pitch(es), how many had "
        f"at least one cue from this source within ±{diag.tolerance_s:.1f}s naming "
        "that pitch's pitcher or batter. This says whether a recognizer works at all; "
        "it is deliberately easier than the gate above and must never be quoted as "
        "accuracy.",
        "",
        "| Source | Pitches w/ pitcher cue | Pitches w/ batter cue | Pitches covered "
        "| Coverage |",
        "|---|---|---|---|---|",
    ]
    for src in sorted(diag.per_source):
        d = diag.per_source[src]
        pct = _pct(d.pitches_covered / diag.scored_pitches if diag.scored_pitches else None)
        lines.append(
            f"| {src} | {d.pitches_with_pitcher_cue} | {d.pitches_with_batter_cue} "
            f"| {d.pitches_covered} | {pct} |"
        )

    off_target = {
        src: near_miss_summary(d.near_miss_s)
        for src, d in diag.per_source.items()
        if d.near_miss_s
    }
    if off_target:
        lines += [
            "",
            "### Near misses (cues that land outside tolerance)",
            "",
            "Distance in seconds from the cue's range to the nearest scored pitch. "
            "Small numbers mean the tolerance or the cue windows are the problem; "
            "moderate ones mean the cue is about something else entirely (a "
            "between-pitch close-up, a dugout shot); distances on the order of the "
            "media duration mean the cue's timestamps are wrong, which is a "
            "plumbing bug, not a recognizer verdict.",
            "",
            "| Source | Off-target cues | min | median | p90 | max |",
            "|---|---|---|---|---|---|",
        ]
        for src in sorted(off_target):
            s = off_target[src]
            lines.append(
                f"| {src} | {int(s['count'])} | {s['min']:.1f} | {s['median']:.1f} "
                f"| {s['p90']:.1f} | {s['max']:.1f} |"
            )

    unknown = {src: d.unknown_entity_ids for src, d in diag.per_source.items()
               if d.unknown_entity_ids}
    if unknown:
        lines += ["", "### Entity ids not on the roster", ""]
        for src in sorted(unknown):
            ids = ", ".join(f"`{i}`" for i in sorted(unknown[src])[:DEBUG_SAMPLE])
            lines.append(f"- **{src}**: {ids}")
        lines.append("")
        lines.append(
            "These cues can never match a roster-keyed event; the recognizer and the "
            "roster disagree on the entity vocabulary."
        )

    if debug:
        lines += ["", "### Sample cues (--debug)", ""]
        for src in sorted(diag.per_source):
            d = diag.per_source[src]
            lines.append(f"**{src}** (first {len(d.samples)} of {d.cues})")
            lines.append("")
            for cue in d.samples:
                names = ", ".join(
                    (roster.get(pid).name if roster.get(pid) else pid)
                    for pid in cue.entity_ids
                )
                lines.append(
                    f"- `{cue.start_s:.1f}-{cue.end_s:.1f}s` {cue.event} "
                    f"conf {cue.confidence:.2f}: {names or '(no entity)'}"
                )
            lines.append("")
    lines.append("")
    return lines
