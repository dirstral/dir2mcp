import json
from pathlib import Path

import pytest

from dirstral_annotator.eval import ground_truth
from dirstral_annotator.eval.align import Anchor, estimate
from dirstral_annotator.eval.report import render
from dirstral_annotator.eval.score import score
from dirstral_annotator.model import Annotation
from dirstral_annotator.roster import Roster

FIXTURE = Path(__file__).parent / "fixtures" / "gumbo_min.json"


@pytest.fixture
def events():
    return ground_truth.parse_pitches(ground_truth.load_game(FIXTURE))


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": 657277},
    ]))
    return Roster.load(path)


def test_parse_pitches(events):
    # 5 pitch events in fixture, one with no timestamp -> skipped
    assert len(events) == 4
    assert all(e.pitcher_id == 657277 for e in events)
    assert events[0].description == "Called Strike"
    # last pitch of an at-bat carries the play result text
    assert events[1].description == "Freddie Freeman flies out to center field."
    assert events == sorted(events, key=lambda e: e.epoch_s)


def test_alignment_median_and_drift():
    a = estimate([Anchor(epoch_s=1000.0, video_s=100.0)])
    assert a.offset_s == -900.0 and a.spread_s == 0.0 and not a.drifty
    assert a.to_video(1010.0) == 110.0

    b = estimate([
        Anchor(1000.0, 100.0), Anchor(2000.0, 1100.1), Anchor(3000.0, 2100.2),
    ])
    assert abs(b.offset_s - (-899.9)) < 1e-6  # median
    assert not b.drifty

    c = estimate([Anchor(1000.0, 100.0), Anchor(2000.0, 1110.0)])
    assert c.drifty  # 10s spread: spliced timeline


def make_annotation(events, alignment, i, entity="player:webb-logan"):
    t = alignment.to_video(events[i].epoch_s)
    return Annotation(start_s=t - 3, end_s=t + 5, event="pitch",
                      entity_ids=(entity,), text="", confidence=0.98,
                      sources=("playbyplay",))


def test_score_perfect_recall(events, roster):
    alignment = estimate([Anchor(events[0].epoch_s, 60.0)])
    anns = [make_annotation(events, alignment, i) for i in range(4)]
    card = score(anns, events, alignment, roster)
    assert card.total_events == 4
    assert card.overall.recall == 1.0 and card.overall.precision == 1.0
    assert card.per_source_found["playbyplay"] == 4


def test_score_misses_and_false_positives(events, roster):
    alignment = estimate([Anchor(events[0].epoch_s, 60.0)])
    anns = [
        make_annotation(events, alignment, 0),
        # false positive: a "pitch" far from any real one
        Annotation(start_s=5000, end_s=5010, event="pitch",
                   entity_ids=("player:webb-logan",), text="", confidence=0.5,
                   sources=("face",)),
    ]
    card = score(anns, events, alignment, roster)
    assert card.overall.tp == 1 and card.overall.fn == 3 and card.overall.fp == 1
    assert card.overall.recall == 0.25
    assert card.overall.precision == 0.5


def test_report_renders(events, roster):
    alignment = estimate([Anchor(events[0].epoch_s, 60.0)])
    anns = [make_annotation(events, alignment, i) for i in range(4)]
    card = score(anns, events, alignment, roster)
    text = render(card, alignment, roster, title="game7.mp4")
    assert "Logan Webb" in text and "100.0%" in text
    assert "DRIFT" not in text
