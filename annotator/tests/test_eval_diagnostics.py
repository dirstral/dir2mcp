"""Diagnostics that tell "vision is weak" apart from "vision is invisible".

The pilot's headline (99.4% recall, 100% precision, playbyplay the only listed
source) was read as "the three vision recognizers found nothing". These tests
pin the actual reason: the metric scores `pitch` annotations naming the
ground-truth pitcher, vision emits `at_bat` / `appearance` cues naming whoever
is on screen, and fusion never merges across event types, so a vision cue
cannot reach the table no matter how good it is.
"""

import json
from pathlib import Path

import pytest

from dirstral_annotator.eval import ground_truth
from dirstral_annotator.eval.align import Anchor, estimate
from dirstral_annotator.eval.diagnose import diagnose, run_pipeline, scored_windows
from dirstral_annotator.eval.report import render
from dirstral_annotator.eval.score import score
from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Cue
from dirstral_annotator.pipeline import GameConfig, Pipeline
from dirstral_annotator.roster import Roster

FIXTURE = Path(__file__).parent / "fixtures" / "gumbo_min.json"
PITCHER = "player:webb-logan"
BATTER = "player:freeman-freddie"


@pytest.fixture
def events():
    return ground_truth.parse_pitches(ground_truth.load_game(FIXTURE))


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": PITCHER, "name": "Logan Webb", "number": "62", "mlbam_id": 657277},
        {"id": BATTER, "name": "Freddie Freeman", "number": "5", "mlbam_id": 518692},
    ]))
    return Roster.load(path)


@pytest.fixture
def alignment(events):
    return estimate([Anchor(events[0].epoch_s, 60.0)])


def pitch_cue(alignment, events, i, entity=PITCHER, source="playbyplay", conf=0.98):
    t = alignment.to_video(events[i].epoch_s)
    return Cue(source=source, start_s=t - 3, end_s=t + 5, event="pitch",
               entity_ids=(entity,), confidence=conf, text="")


def vision_cue(alignment, events, i, entity=BATTER, source="scorebug",
               event="at_bat", conf=0.6, shift=0.0):
    t = alignment.to_video(events[i].epoch_s) + shift
    return Cue(source=source, start_s=t - 2, end_s=t + 2, event=event,
               entity_ids=(entity,), confidence=conf, text="")


def run(cues, events, alignment, roster, min_confidence=0.0):
    annotations = fuse(cues, min_confidence=min_confidence)
    card = score(annotations, events, alignment, roster)
    diag = diagnose(cues, annotations, events, alignment, roster, card,
                    min_confidence=min_confidence)
    return card, diag


# --- the root cause -------------------------------------------------------


def test_fusion_never_merges_across_event_types(alignment, events):
    """A perfectly co-timed `at_bat` cue cannot join a `pitch` annotation.

    This is the mechanism behind the empty source table: fusion groups by
    event, so no vision cue can ever end up in `Annotation.sources` of the
    annotations the scorer reads.
    """
    cues = [pitch_cue(alignment, events, 0),
            vision_cue(alignment, events, 0, entity=PITCHER)]
    annotations = fuse(cues)
    assert len(annotations) == 2
    by_event = {a.event: a for a in annotations}
    assert by_event["pitch"].sources == ("playbyplay",)
    assert by_event["at_bat"].sources == ("scorebug",)


def test_vision_source_is_unreachable_not_weak(alignment, events, roster):
    """Scorebug sees every pitch, at the right time, and still scores zero."""
    cues = [pitch_cue(alignment, events, i) for i in range(len(events))]
    cues += [vision_cue(alignment, events, i) for i in range(len(events))]
    card, diag = run(cues, events, alignment, roster)

    assert card.overall.recall == 1.0
    assert dict(card.per_source_found) == {"playbyplay": 4}  # the misleading table

    sb = diag.per_source["scorebug"]
    assert sb.cues == 4  # cues were emitted
    assert sb.annotations_fused == 4 and sb.annotations_dropped == 0  # and survived
    assert sb.cues_near_pitch == 4  # and landed on real pitches
    assert sb.scored_event_cues == 0  # but are not the event the metric reads
    assert sb.found_pitches == 0
    assert sb.unreachable
    assert "UNREACHABLE" in sb.verdict and "at_bat" in sb.verdict
    assert diag.unreachable_sources == ["scorebug"]


def test_pitch_event_source_that_never_names_the_pitcher_is_unreachable(
    alignment, events, roster
):
    """Same trap one level down: right event, wrong role.

    A recognizer emitting `pitch` cues about the batter clears the event
    filter and still cannot score, because the metric is pitcher-keyed.
    """
    cues = [vision_cue(alignment, events, i, entity=BATTER, source="jersey",
                       event="pitch") for i in range(len(events))]
    _, diag = run(cues, events, alignment, roster)
    j = diag.per_source["jersey"]
    assert j.scored_event_cues == 4 and j.cues_near_pitch == 4
    # Two of the four fixture pitches are to a rostered batter (Freeman); the
    # other two are to Betts, who is not on this roster.
    assert j.cues_naming_pitcher == 0 and j.cues_naming_batter == 2
    assert j.unreachable and "pitcher-keyed" in j.verdict


# --- the cases the diagnostics must NOT confuse with unreachability -------


def test_genuinely_weak_source_reads_as_weak(alignment, events, roster):
    """Eligible in every structural way, simply late: verdict is `weak`."""
    cues = [Cue(source="face", start_s=5000, end_s=5010, event="pitch",
                entity_ids=(PITCHER,), confidence=0.6)]
    _, diag = run(cues, events, alignment, roster)
    f = diag.per_source["face"]
    assert not f.unreachable
    assert f.cues_near_pitch == 0
    assert f.verdict.startswith("weak")
    assert f.near_miss_s and f.near_miss_s[0] > 1000  # far from any pitch


def test_near_miss_distance_shows_a_tolerance_problem(alignment, events, roster):
    """A cue 8s off a pitch is a tolerance question, not a vision failure."""
    cues = [vision_cue(alignment, events, 0, entity=PITCHER, source="face",
                       event="pitch", shift=8.0)]
    _, diag = run(cues, events, alignment, roster)
    f = diag.per_source["face"]
    assert f.cues_near_pitch == 0
    # The cue spans t+6..t+10: 6s from the pitch, just past the 5s tolerance.
    # A 6s number says "widen the tolerance or the cue window"; a 600s number
    # would say "this recognizer is looking at something else".
    assert f.near_miss_s == pytest.approx([6.0], abs=0.01)


def test_confidence_floor_is_reported_as_dropped(alignment, events, roster):
    cues = [pitch_cue(alignment, events, 0)]
    cues += [vision_cue(alignment, events, i, source="face", event="appearance",
                        conf=0.2) for i in range(len(events))]
    _, diag = run(cues, events, alignment, roster, min_confidence=0.5)
    f = diag.per_source["face"]
    assert f.cues == 4
    assert f.annotations_fused == 0 and f.annotations_dropped > 0
    assert f.verdict.startswith("dropped")


def test_cue_span_exposes_corrupt_timestamps(alignment, events, roster):
    """Cues running past the end of the media are a plumbing fault.

    Observed for real: with `--jersey --faces` together, the face recognizer's
    cues ran to 6446s on a 900s clip, and every per-pitch number for it was
    meaningless. A per-pitch count cannot show that; the cue span can.
    """
    cues = [Cue(source="face", start_s=t, end_s=t + 4, event="appearance",
                entity_ids=(PITCHER,), confidence=0.6)
            for t in (100.0, 6400.0)]
    card, diag = run(cues, events, alignment, roster)
    f = diag.per_source["face"]
    assert (f.first_cue_s, f.last_cue_s) == (100.0, 6404.0)
    text = render(card, alignment, roster, title="clip.mp4", diagnostics=diag)
    assert "100..6404" in text
    assert "corrupt timestamps" in text


def test_entity_ids_off_the_roster_are_surfaced(alignment, events, roster):
    cues = [Cue(source="jersey", start_s=57, end_s=65, event="appearance",
                entity_ids=("player:not-on-this-roster",), confidence=0.6)]
    _, diag = run(cues, events, alignment, roster)
    assert diag.per_source["jersey"].unknown_entity_ids == {"player:not-on-this-roster"}


# --- the honest, clearly-labelled vision number ---------------------------


def test_identity_coverage_credits_batter_sightings(alignment, events, roster):
    """The number that says whether vision works, kept out of the gate."""
    cues = [vision_cue(alignment, events, i) for i in range(2)]
    _, diag = run(cues, events, alignment, roster)
    sb = diag.per_source["scorebug"]
    assert diag.scored_pitches == 4
    assert sb.pitches_with_batter_cue == 2 and sb.pitches_with_pitcher_cue == 0
    assert sb.pitches_covered == 2


def test_coverage_counts_each_pitch_once(alignment, events, roster):
    """A chatty recognizer cannot inflate coverage by repeating itself."""
    cues = [vision_cue(alignment, events, 0) for _ in range(10)]
    _, diag = run(cues, events, alignment, roster)
    assert diag.per_source["scorebug"].pitches_covered == 1


def test_scored_windows_mirror_the_scorer(events, alignment, roster, tmp_path):
    """Diagnostics must use the scorer's denominator, not a different one."""
    path = tmp_path / "pitcherless.json"
    path.write_text(json.dumps([{"id": BATTER, "name": "Freddie Freeman",
                                 "mlbam_id": 518692}]))
    pitcherless = Roster.load(path)
    assert scored_windows(events, alignment, pitcherless) == []
    assert len(scored_windows(events, alignment, roster)) == 4


# --- report + wiring ------------------------------------------------------


def test_report_states_the_unreachability(alignment, events, roster):
    cues = [pitch_cue(alignment, events, i) for i in range(len(events))]
    cues += [vision_cue(alignment, events, i) for i in range(len(events))]
    card, diag = run(cues, events, alignment, roster)
    text = render(card, alignment, roster, title="game7.mp4", diagnostics=diag,
                  debug=True)
    assert "cannot score on this metric" in text
    assert "has not been measured as weak" in text
    assert "Identity coverage" in text
    assert "NOT the phase-1 gate" in text
    assert "Sample cues" in text
    # the headline stays where it was, with its scope spelled out
    assert "recall: **100.0%**" in text
    assert "mostly a measurement of wall-clock alignment" in text


def test_report_without_diagnostics_is_unchanged(alignment, events, roster):
    cues = [pitch_cue(alignment, events, i) for i in range(len(events))]
    card, _ = run(cues, events, alignment, roster)
    text = render(card, alignment, roster, title="game7.mp4")
    assert "Source diagnostics" not in text


def test_run_pipeline_keeps_the_cues(tmp_path, roster, events):
    """`Pipeline.annotations_for` discards cues; the eval path must not."""
    media = tmp_path / "game7.mp4"
    media.write_bytes(b"\x00")
    game = GameConfig.parse({
        "feed": str(FIXTURE), "anchors": [f"{events[0].epoch_s}=60.0"],
    })
    pipeline = Pipeline(roster=roster, games={media.name: game})
    cues, annotations = run_pipeline(pipeline, media)
    assert cues and annotations
    assert {c.event for c in cues} == {"pitch", "at_bat"}
    assert annotations == pipeline.annotations_for(media)


def test_eval_cli_emits_diagnostics(tmp_path, events):
    from dirstral_annotator.cli import main

    media = tmp_path / "game7.mp4"
    media.write_bytes(b"\x00")
    roster_path = tmp_path / "cli-roster.json"
    roster_path.write_text(json.dumps([
        {"id": PITCHER, "name": "Logan Webb", "number": "62", "mlbam_id": 657277},
    ]))
    report = tmp_path / "report.md"
    rc = main(["eval", str(media), "--roster", str(roster_path), "--feed", str(FIXTURE),
               "--anchor", f"{events[0].epoch_s}=60.0", "--report", str(report)])
    assert rc == 0
    text = report.read_text()
    assert "## Source diagnostics" in text
    assert "playbyplay" in text
