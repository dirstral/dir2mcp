"""Render a Scorecard as the phase-1 markdown accuracy report."""

from __future__ import annotations

from ..roster import Roster
from .align import Alignment
from .score import Scorecard


def _pct(v: float | None) -> str:
    return f"{v * 100:.1f}%" if v is not None else "n/a"


def render(card: Scorecard, alignment: Alignment, roster: Roster, title: str) -> str:
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
        "| Source | Found pitches it contributed to |",
        "|---|---|",
    ]
    for src in sorted(card.per_source_found):
        lines.append(f"| {src} | {card.per_source_found[src]} |")
    if not card.per_source_found:
        lines.append("| _none_ | 0 |")
    lines.append("")
    return "\n".join(lines)
