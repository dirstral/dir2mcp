import json
from collections import Counter
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


def test_one_annotation_covers_at_most_one_event(events, roster):
    # A single wide annotation spanning two distinct ground-truth pitches must
    # satisfy only ONE of them — the recall loop skips already-matched
    # annotations, so a broad prediction cannot inflate recall.
    alignment = estimate([Anchor(events[0].epoch_s, 60.0)])
    t0 = alignment.to_video(events[0].epoch_s)
    t1 = alignment.to_video(events[1].epoch_s)
    assert t0 != t1  # the two events map to distinct video times
    lo, hi = sorted((t0, t1))
    wide = Annotation(start_s=lo - 1, end_s=hi + 1, event="pitch",
                      entity_ids=("player:webb-logan",), text="", confidence=0.9,
                      sources=("playbyplay",))
    card = score([wide], events, alignment, roster)
    assert card.overall.tp == 1  # not 2 — one annotation, one event
    assert card.per_source_found["playbyplay"] == 1


def test_report_renders(events, roster):
    alignment = estimate([Anchor(events[0].epoch_s, 60.0)])
    anns = [make_annotation(events, alignment, i) for i in range(4)]
    card = score(anns, events, alignment, roster)
    text = render(card, alignment, roster, title="game7.mp4")
    assert "Logan Webb" in text and "100.0%" in text
    assert "DRIFT" not in text


# --- club names off the feed (#741 follow-up) --------------------------------

def _feed_with_clubs(top_inning: bool) -> dict:
    """A minimal GUMBO payload carrying both clubs and one pitch."""
    return {
        "gamePk": 1,
        "gameData": {"teams": {
            "away": {"name": "Washington Nationals"},
            "home": {"name": "San Francisco Giants"},
        }},
        "liveData": {"plays": {"allPlays": [{
            "matchup": {"pitcher": {"id": 1, "fullName": "P"},
                        "batter": {"id": 2, "fullName": "B"}},
            "about": {"inning": 3, "isTopInning": top_inning},
            "result": {"description": ""},
            "playEvents": [{
                "isPitch": True,
                "startTime": "2026-08-01T20:00:00Z",
                "details": {"description": "Ball"},
            }],
        }]}},
    }


def test_parse_pitches_reads_both_clubs():
    ev = ground_truth.parse_pitches(_feed_with_clubs(top_inning=True))[0]
    assert ev.away_team == "Washington Nationals"
    assert ev.home_team == "San Francisco Giants"


def test_the_batting_club_follows_the_half_inning():
    """The visitors bat in the top half. This is the whole derivation, so it
    is worth pinning in both directions rather than trusting one case."""
    top = ground_truth.parse_pitches(_feed_with_clubs(top_inning=True))[0]
    assert top.batting_team() == "Washington Nationals"
    assert top.pitching_team() == "San Francisco Giants"

    bottom = ground_truth.parse_pitches(_feed_with_clubs(top_inning=False))[0]
    assert bottom.batting_team() == "San Francisco Giants"
    assert bottom.pitching_team() == "Washington Nationals"


def test_a_feed_without_gameData_leaves_the_clubs_empty(events):
    """The shared fixture has no `gameData`; every existing caller and payload
    must stay valid, with no club rather than a guessed one."""
    assert all(e.away_team == "" and e.home_team == "" for e in events)
    assert all(e.batting_team() == "" and e.pitching_team() == "" for e in events)


# --- the outcome of a play, as structured data ------------------------------
#
# The outcome used to live only in the prose of `description`. Nothing could
# select on it, so "who hit home runs? list every player" fell back to top-k
# search over that prose and answered a "list every X" question from a partial
# sample. `result.eventType` is the outcome the feed already types, so these
# tests run on the real pilot game rather than on a shape invented here.
#
# Both fixtures are real statsapi payloads
# (`/api/v1.1/game/{game_pk}/feed/live`), projected down to the fields
# `parse_pitches` reads and otherwise unedited:
#
#   gumbo_823215.json         game 823215, the pilot game, all 84 plays
#   gumbo_trailing_action.json game 776815, the one play in 8 games whose last
#                             playEvent is not a pitch

PILOT = Path(__file__).parent / "fixtures" / "gumbo_823215.json"
TRAILING = Path(__file__).parent / "fixtures" / "gumbo_trailing_action.json"

#: What game 823215 (Washington at San Francisco) actually holds. Counted from
#: the live statsapi payload, then projected down to the fields the parser
#: reads; nothing here is chosen, it is what the game was.
PILOT_OUTCOMES = {
    "field_out": 37, "single": 18, "strikeout": 13, "home_run": 6,
    "walk": 5, "double": 4, "force_out": 1,
}
PILOT_PLAYS = 84
PILOT_PITCHES = 344


@pytest.fixture
def pilot_events():
    return ground_truth.parse_pitches(ground_truth.load_game(PILOT))


def test_the_pilot_feed_types_every_outcome(pilot_events):
    """The measurement this change rests on: the outcome is already structured
    in the feed, for every play, with no prose parsing anywhere."""
    assert len(pilot_events) == PILOT_PITCHES
    outcomes = Counter(e.event_type for e in pilot_events if e.event_type)
    assert dict(outcomes) == PILOT_OUTCOMES
    assert sum(outcomes.values()) == PILOT_PLAYS  # one outcome per at-bat


def test_only_the_pitch_that_ended_the_at_bat_carries_the_outcome(pilot_events):
    """A 6 pitch at-bat is not 6 home runs. The outcome rides where the play
    result text already rides: on the pitch that ended the at-bat."""
    ended = [e for e in pilot_events if e.event_type]
    assert len(ended) == PILOT_PLAYS
    # The play result text is a sentence about the whole play ("... homers (9)
    # on a fly ball ..."); a mid-at-bat pitch carries its own call instead.
    assert all(e.description.endswith(".") for e in ended)
    homers = [e for e in pilot_events if e.event_type == "home_run"]
    assert len(homers) == 6
    assert all("homer" in e.description or "grand slam" in e.description
               for e in homers)


def test_a_play_that_does_not_end_on_a_pitch_still_records_its_outcome():
    """The `is_last` hazard, from a real game.

    `ev is play["playEvents"][-1]` was evaluated after the loop had skipped
    every non-pitch, so a play whose last playEvent is not a pitch marked NO
    pitch as last. This fixture is that play, unedited: Aaron Judge walks on an
    "Automatic Ball - Pitcher Pitch Timer Violation", which the feed records
    with `isPitch: false`. One play in 594 across 8 games of 2025, so it is
    rare and real, and each occurrence is an at-bat whose outcome no
    structured query could reach.
    """
    events = ground_truth.parse_pitches(ground_truth.load_game(TRAILING))
    assert events, "the fixture must hold pitches, or this proves nothing"
    ended = [e for e in events if e.event_type]
    assert [e.event_type for e in ended] == ["walk"]
    assert ended[0] is events[-1]  # the last PITCH, not the last playEvent
    assert ended[0].description == "Aaron Judge walks."


def test_a_pitch_with_no_timestamp_cannot_end_an_at_bat():
    """A pitch with no `startTime` is skipped, because it cannot be placed on
    the video timeline. It must not take the outcome with it: the outcome moves
    to the last pitch that a caller can actually reach."""
    feed = _feed_with_clubs(top_inning=True)
    play = feed["liveData"]["plays"]["allPlays"][0]
    play["result"] = {"description": "B strikes out swinging.", "eventType": "strikeout"}
    play["playEvents"].append({"isPitch": True, "details": {"description": "no time"}})
    parsed = ground_truth.parse_pitches(feed)
    assert len(parsed) == 1
    assert parsed[0].event_type == "strikeout"
    assert parsed[0].description == "B strikes out swinging."


def test_a_feed_without_an_eventType_leaves_the_outcome_empty(events):
    """Back-compat, and the reason every existing suite is untouched: the
    shared fixture types no play, so it produces no outcome."""
    assert all(e.event_type == "" for e in events)
