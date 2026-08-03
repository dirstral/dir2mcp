"""Reachable-pitch coverage: separates a weak recognizer from a small gallery.

Raw coverage conflates two failures that call for opposite fixes. A face bank
holding 13 of 52 players and a recognizer that rarely resolves a face both read
as low coverage. `pitches_reachable` uses the source's own emitted identities as
its vocabulary, so the distinction needs no knowledge of the gallery.
"""

from __future__ import annotations

import json

from dirstral_annotator.eval import diagnose
from dirstral_annotator.eval.align import Alignment
from dirstral_annotator.eval.ground_truth import PitchEvent
from dirstral_annotator.eval.score import Scorecard
from dirstral_annotator.model import Cue
from dirstral_annotator.roster import Roster


def _roster(tmp_path, n):
    rows = [
        {"id": f"player:p{i}", "name": f"P{i}", "aliases": [f"P{i}"], "mlbam_id": 1000 + i}
        for i in range(n)
    ]
    path = tmp_path / "roster.json"
    path.write_text(json.dumps(rows))
    return Roster.load(path)


def _events(pitcher_idx, batter_idx):
    """One pitch every 10s, players identified by roster index."""
    return [
        PitchEvent(game_pk=1, epoch_s=float(i) * 10, pitcher_id=1000 + p,
                   pitcher_name=f"P{p}", batter_id=1000 + b, batter_name=f"P{b}",
                   inning=1, description="")
        for i, (p, b) in enumerate(zip(pitcher_idx, batter_idx))
    ]


def _cue(source, ids, start):
    return Cue(source=source, start_s=start, end_s=start + 1.0, event="appearance",
               entity_ids=tuple(ids), confidence=0.9, text="")


def _run(tmp_path, pitcher_idx, batter_idx, cues, n_players=40):
    return diagnose.diagnose(
        cues=cues, annotations=[], events=_events(pitcher_idx, batter_idx),
        alignment=Alignment(offset_s=0.0, spread_s=0.0, anchors=1),
        roster=_roster(tmp_path, n_players), card=Scorecard(),
    )


def test_narrow_vocabulary_shrinks_the_reachable_set(tmp_path):
    """A source that only ever names one player can only reach the pitches that
    player appears in. That is the enrollment-limited signature."""
    diag = _run(tmp_path, [0] * 5 + [1] * 5, list(range(10, 20)),
                [_cue("narrow", ["player:p0"], 0.0)])
    b = diag.per_source["narrow"]
    assert b.pitches_reachable == 5, "only p0's 5 pitches are inside its vocabulary"
    assert b.pitches_covered == 1


def test_broad_vocabulary_keeps_the_reachable_set_large(tmp_path):
    """A source naming many players reaches most pitches, so low coverage there
    means it identifies rarely, not narrowly: the recognizer-limited signature."""
    diag = _run(tmp_path, list(range(10)), list(range(10, 20)),
                [_cue("broad", [f"player:p{i}"], float(i) * 10) for i in range(10)])
    b = diag.per_source["broad"]
    assert b.pitches_reachable == 10
    assert b.pitches_covered == 10


def test_a_source_naming_nobody_reaches_nothing(tmp_path):
    diag = _run(tmp_path, [0] * 4, [1] * 4, [_cue("silent", [], 0.0)])
    b = diag.per_source["silent"]
    assert b.pitches_reachable == 0
    assert b.pitches_covered == 0
