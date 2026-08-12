"""Play-by-play recognizer: role-aware event typing.

A pitch involves two rostered-relevant roles. The pitcher *threw* it (a
`pitch` event, which the pitcher-keyed phase-1 metric scores); the batter
*faced* it (an `at_bat` appearance, which must NOT enter the pitch pool). The
regression these tests lock down: a rostered batter facing an opponent's
pitch used to be emitted as a `pitch` cue, turning every opponent-pitched
at-bat into a precision false positive and flooring precision to ~50% even
with a perfect metadata source.
"""

import json
from pathlib import Path

import pytest

from dirstral_annotator.eval import ground_truth
from dirstral_annotator.eval.align import Anchor, estimate
from dirstral_annotator.eval.ground_truth import PitchEvent
from dirstral_annotator.eval.score import score
from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Player, slug_entity_id
from dirstral_annotator.recognizers.playbyplay import PlayByPlayRecognizer
from dirstral_annotator.roster import Roster

MEDIA = Path("game.mp4")  # unused by the recognizer: labels are external

# roster: one of our pitchers, one of our position players
WEBB = 657277
RAMOS = 671218
OPP_PITCHER = 111111
OPP_BATTER = 222222


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": WEBB},
        {"id": "player:ramos-heliot", "name": "Heliot Ramos", "number": "17", "mlbam_id": RAMOS},
    ]))
    return Roster.load(path)


def _pitch(epoch, pitcher, batter, pname="P", bname="B", desc="Ball"):
    return PitchEvent(
        game_pk=1, epoch_s=epoch, pitcher_id=pitcher, pitcher_name=pname,
        batter_id=batter, batter_name=bname, inning=1, description=desc,
    )


def test_rostered_pitcher_emits_pitch(roster):
    ev = _pitch(1000.0, WEBB, OPP_BATTER, pname="Logan Webb", bname="Opp Guy")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 1
    assert cues[0].event == "pitch"
    assert cues[0].entity_ids == ("player:webb-logan",)
    assert "Logan Webb" in cues[0].text


def test_rostered_batter_emits_at_bat_not_pitch(roster):
    # opponent pitching to our batter -> at_bat, NEVER pitch
    ev = _pitch(1000.0, OPP_PITCHER, RAMOS, pname="Opp Ace", bname="Heliot Ramos")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 1
    assert cues[0].event == "at_bat"
    assert cues[0].entity_ids == ("player:ramos-heliot",)
    assert "Heliot Ramos" in cues[0].text


def test_our_pitcher_facing_our_batter_splits_into_two_events(roster):
    # intrasquad edge: both rostered -> one pitch (pitcher) + one at_bat (batter)
    ev = _pitch(1000.0, WEBB, RAMOS)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert len(cues) == 2  # exactly one pitch + one at_bat, no duplicates
    assert {(c.event, c.entity_ids) for c in cues} == {
        ("pitch", ("player:webb-logan",)),
        ("at_bat", ("player:ramos-heliot",)),
    }


def test_no_rostered_participant_skipped(roster):
    ev = _pitch(1000.0, OPP_PITCHER, OPP_BATTER)
    assert PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA) == []


def test_batter_appearance_is_not_a_precision_false_positive(roster):
    # THE REGRESSION: a game that is half our-pitcher, half our-batter. The
    # at-bats must not be scored as pitch false positives -> precision stays
    # 1.0, not ~0.5.
    events = [
        _pitch(1000.0, WEBB, OPP_BATTER, pname="Logan Webb"),        # our pitch
        _pitch(1100.0, OPP_PITCHER, RAMOS, bname="Heliot Ramos"),    # our at-bat
        _pitch(1200.0, WEBB, OPP_BATTER, pname="Logan Webb"),        # our pitch
        _pitch(1300.0, OPP_PITCHER, RAMOS, bname="Heliot Ramos"),    # our at-bat
    ]
    alignment = estimate([Anchor(events[0].epoch_s, 5.0)])
    cues = PlayByPlayRecognizer(events, alignment.offset_s, roster).recognize(MEDIA)
    anns = fuse(cues)
    card = score(anns, events, alignment, roster)
    # only Webb's two pitches are rostered-pitcher events
    assert card.total_events == 2
    assert card.overall.recall == 1.0
    assert card.overall.precision == 1.0  # at-bats stay out of the pitch pool


# --- the text a person's question has to match ------------------------------

def test_cue_text_names_the_half_inning(roster):
    """"Bottom of the 9th" is how a moment gets asked for. The inning was parsed
    from the feed and dropped before the text, so no inning query could match
    anything the index held (#741)."""
    ev = PitchEvent(
        game_pk=1, epoch_s=1000.0, pitcher_id=WEBB, pitcher_name="Logan Webb",
        batter_id=RAMOS, batter_name="Heliot Ramos", inning=7, top_inning=False,
        description="Heliot Ramos strikes out swinging.",
    )
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(Path("g.mp4"))
    pitch = next(c for c in cues if c.event == "pitch")
    assert "bottom of the 7th" in pitch.text
    # The outcome and both names survive alongside it: a query is as likely to
    # be "Ray strikeout" as it is to be an inning.
    assert "Logan Webb" in pitch.text and "Heliot Ramos" in pitch.text
    assert "strikes out swinging" in pitch.text


def test_the_ordinal_is_right_for_the_teens():
    """Extra innings are ordinary in baseball, and 11th/12th/13th are exactly
    where a naive ordinal says "11st"."""
    from dirstral_annotator.eval.ground_truth import _ordinal
    assert [_ordinal(n) for n in (1, 2, 3, 11, 12, 13, 21, 22)] == \
        ["1st", "2nd", "3rd", "11th", "12th", "13th", "21st", "22nd"]


def test_a_missing_inning_is_omitted_rather_than_guessed(roster):
    """A feed without an inning should read as a moment with no inning stated,
    not as "top of the 0th"."""
    ev = PitchEvent(
        game_pk=1, epoch_s=1000.0, pitcher_id=WEBB, pitcher_name="Logan Webb",
        batter_id=RAMOS, batter_name="Heliot Ramos", inning=0,
        description="Heliot Ramos grounds out.",
    )
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(Path("g.mp4"))
    pitch = next(c for c in cues if c.event == "pitch")
    assert "0th" not in pitch.text and "(" not in pitch.text


# --- club attribution (#741 follow-up; dirstral-spec design 0004 §6.1) -------
#
# The club a player is acting for rides in `entity_ids`, NOT in the cue text.
# That is a measured decision, not a stylistic one: writing the club into the
# text was tried on the pilot corpus and made retrieval worse, because every
# statement names both clubs and the label then drags a team-scoped query onto
# whichever role ranks first. As an entity it is exact, because `event`
# already records the role the id is acting in.

BOTH_CLUBS = {"away_team": "Washington Nationals", "home_team": "San Francisco Giants"}


def _pitch_with_clubs(pitcher, batter, *, top_inning, **kw):
    return PitchEvent(
        game_pk=1, epoch_s=1000.0, pitcher_id=pitcher, pitcher_name="P",
        batter_id=batter, batter_name="B", inning=1, top_inning=top_inning,
        description="Ball", **{**BOTH_CLUBS, **kw},
    )


def test_the_pitch_carries_the_fielding_club_and_the_at_bat_the_batting_club(roster):
    """Bottom half: the home side bats, so our rostered batter is a Giant and
    the pitcher he faces is fielding for Washington."""
    ev = _pitch_with_clubs(OPP_PITCHER, RAMOS, top_inning=False)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    at_bat = next(c for c in cues if c.event == "at_bat")
    assert at_bat.entity_ids == ("player:ramos-heliot", "team:san-francisco-giants")


def test_the_half_inning_decides_which_club_is_batting(roster):
    """Top half: the visitors bat. Our rostered pitcher is therefore fielding
    for the home club. Getting this backwards would attribute every moment to
    the wrong side, which no amount of ranking could recover."""
    ev = _pitch_with_clubs(WEBB, OPP_BATTER, top_inning=True)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    pitch = next(c for c in cues if c.event == "pitch")
    assert pitch.entity_ids == ("player:webb-logan", "team:san-francisco-giants")

    # ... and in the bottom half the same pitcher would be fielding for the
    # visitors, so the club is genuinely derived rather than constant.
    flipped = _pitch_with_clubs(WEBB, OPP_BATTER, top_inning=False)
    pitch = next(
        c for c in PlayByPlayRecognizer([flipped], 0.0, roster).recognize(MEDIA)
        if c.event == "pitch"
    )
    assert pitch.entity_ids == ("player:webb-logan", "team:washington-nationals")


def test_both_roles_in_one_pitch_get_opposite_clubs(roster):
    """The case that makes the entity filter role-exact: one moment, two
    annotations, each naming the club its own actor plays for."""
    ev = _pitch_with_clubs(WEBB, RAMOS, top_inning=True)
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    by_event = {c.event: c.entity_ids for c in cues}
    assert by_event["pitch"] == ("player:webb-logan", "team:san-francisco-giants")
    assert by_event["at_bat"] == ("player:ramos-heliot", "team:washington-nationals")


def test_the_club_stays_out_of_the_cue_text(roster):
    """The measured decision. If a club name ever appears in the text, the
    retrieval regression that motivated this comes straight back."""
    ev = _pitch_with_clubs(WEBB, RAMOS, top_inning=True)
    for cue in PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA):
        assert "Giants" not in cue.text
        assert "Nationals" not in cue.text


def test_a_feed_without_clubs_emits_exactly_what_it_did_before(roster):
    """Back-compat: club names are optional, and a payload lacking them must
    not produce a `team:` id with nothing in it."""
    ev = _pitch(1000.0, WEBB, OPP_BATTER, pname="Logan Webb", bname="Opp Guy")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert cues[0].entity_ids == ("player:webb-logan",)


def test_the_club_id_round_trips_through_the_emit_label_fallback():
    """A club has no roster entry, so its label comes from the emit fallback
    (`id.split(":")[-1].replace("-", " ").title()`). The slug has to survive
    that trip, or the wire carries an id nobody can read."""
    from dirstral_annotator.recognizers.playbyplay import team_id
    tid = team_id("San Francisco Giants")
    assert tid == "team:san-francisco-giants"
    assert tid.split(":", 1)[-1].replace("-", " ").title() == "San Francisco Giants"


def test_a_club_name_that_slugs_to_nothing_is_dropped():
    from dirstral_annotator.recognizers.playbyplay import team_id
    assert team_id("") == ""
    assert team_id("   ") == ""
    assert team_id("!!!") == ""


# --- the outcome of an at-bat is an event, not a sentence --------------------
#
# THE DEFECT. Asked "who hit home runs? list every player", the pilot answered
# "Bryce Eldridge, Curtis Mead, Matt Chapman": it invented a player and dropped
# one who really homered. It was not the model (mistral-large-latest gave the
# same wrong list) and not retrieval depth (k=8, k=30 and k=50 all did). Each
# span carried event="at_bat" and the outcome only inside the prose of the cue
# text, so the `events` filter could not reach it, the question fell back to
# top-k semantic search over prose, and a "list every X" question was answered
# from a partial sample: 3 of 14 retrieved hits mentioned a homer, and one
# hitter's chunk was never retrieved at all.
#
# The feed types every play. So the outcome becomes an event, and the question
# becomes a selection instead of a sample.

PILOT = Path(__file__).parent / "fixtures" / "gumbo_823215.json"

#: Game 823215, Washington at San Francisco: what the feed records.
PILOT_HOME_RUNS = 6
PILOT_STRIKEOUTS = 13
PILOT_HOME_RUN_HITTERS = {
    "player:james-wood", "player:matt-chapman", "player:rafael-devers",
    "player:curtis-mead", "player:bryce-eldridge",
}


@pytest.fixture
def pilot_events():
    return ground_truth.parse_pitches(ground_truth.load_game(PILOT))


@pytest.fixture
def pilot_roster(pilot_events):
    """Both clubs, off the feed itself. The pilot roster covers both sides,
    because a question about the game is not a question about one club."""
    players, mlbam = [], {}
    for ev in pilot_events:
        for pid, name in ((ev.pitcher_id, ev.pitcher_name),
                          (ev.batter_id, ev.batter_name)):
            if pid and pid not in mlbam:
                player = Player(id=slug_entity_id(name), name=name)
                mlbam[pid] = player.id
                players.append(player)
    return Roster(players, mlbam)


def _outcome(event_type, **kw):
    """The last pitch of an at-bat: the one the feed types."""
    fields = {
        "game_pk": 1, "epoch_s": 1000.0,
        "pitcher_id": OPP_PITCHER, "pitcher_name": "Opp Ace",
        "batter_id": RAMOS, "batter_name": "Heliot Ramos",
        "inning": 9, "top_inning": False,
        "description": "Heliot Ramos homers (9) on a fly ball to center field.",
        "event_type": event_type, **BOTH_CLUBS,
    }
    return PitchEvent(**{**fields, **kw})


def test_the_outcome_becomes_its_own_event_keyed_on_the_batter(roster):
    """The whole fix in one assertion: the outcome is selectable."""
    cues = PlayByPlayRecognizer([_outcome("home_run")], 0.0, roster).recognize(MEDIA)
    homer = next(c for c in cues if c.event == "home_run")
    assert homer.entity_ids == ("player:ramos-heliot", "team:san-francisco-giants")
    assert homer.start_s == 997.0 and homer.end_s == 1005.0  # the ending pitch


def test_the_outcome_cue_reads_as_a_sentence(roster):
    """`event` is the feed's exact token, so a filter can select it. The text
    stays something a person reads, with the inning and both names, because it
    is also the passage an answer quotes."""
    cues = PlayByPlayRecognizer([_outcome("home_run")], 0.0, roster).recognize(MEDIA)
    homer = next(c for c in cues if c.event == "home_run")
    assert homer.text.startswith("Home run: Heliot Ramos vs Opp Ace")
    assert "bottom of the 9th" in homer.text
    assert "homers (9) on a fly ball" in homer.text
    assert "Giants" not in homer.text  # the club stays an entity, as before


def test_the_outcome_is_an_extra_cue_and_changes_no_existing_one(roster):
    """THE CONTRACT. #620 was caused by retagging, so this change may only add.
    The `pitch` and `at_bat` cues must be identical with and without it."""
    with_outcome = PlayByPlayRecognizer(
        [_outcome("home_run", pitcher_id=WEBB, pitcher_name="Logan Webb")],
        0.0, roster,
    ).recognize(MEDIA)
    without = PlayByPlayRecognizer(
        [_outcome("", pitcher_id=WEBB, pitcher_name="Logan Webb")], 0.0, roster
    ).recognize(MEDIA)

    assert [c.event for c in without] == ["pitch", "at_bat"]
    assert [c.event for c in with_outcome] == ["pitch", "at_bat", "home_run"]
    assert with_outcome[:2] == without  # byte-for-byte, entities and text too


def test_a_pitch_that_does_not_end_the_at_bat_emits_no_outcome(roster):
    """One at-bat is one outcome. A 6 pitch home run at-bat is not 6 homers."""
    mid = _outcome("", description="Foul")  # a pitch inside the at-bat
    cues = PlayByPlayRecognizer([mid], 0.0, roster).recognize(MEDIA)
    assert [c.event for c in cues] == ["at_bat"]


def test_an_unrostered_batter_gets_no_outcome_cue(roster):
    """The outcome is the batter's, so it needs a batter to name. A rostered
    pitcher facing an unrostered batter still emits his `pitch` cue."""
    ev = _outcome("strikeout", pitcher_id=WEBB, pitcher_name="Logan Webb",
                  batter_id=OPP_BATTER, batter_name="Opp Guy")
    cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
    assert [c.event for c in cues] == ["pitch"]


def test_an_outcome_of_whitespace_is_not_an_event(roster):
    """An empty or blank `eventType` must not reach the wire as `event: ""`,
    which no filter could select and no reader could understand."""
    for blank in ("", "   "):
        cues = PlayByPlayRecognizer([_outcome(blank)], 0.0, roster).recognize(MEDIA)
        assert [c.event for c in cues] == ["at_bat"]


def test_an_outcome_may_never_take_a_role_event_name(roster):
    """The #620 guard. A batter-keyed cue tagged `pitch` would enter the
    pitcher-keyed metric as a false positive. MLB types no play "pitch" or
    "at_bat", so this cannot happen today; the metric must not depend on that
    staying true."""
    for stolen in ("pitch", "at_bat"):
        ev = _outcome(stolen, pitcher_id=WEBB, pitcher_name="Logan Webb")
        cues = PlayByPlayRecognizer([ev], 0.0, roster).recognize(MEDIA)
        assert [c.event for c in cues] == ["pitch", "at_bat"]
        assert cues[0].entity_ids[0] == "player:webb-logan"  # still the pitcher


def test_the_outcome_label_spells_the_feeds_own_vocabulary():
    from dirstral_annotator.recognizers.playbyplay import outcome_label
    assert outcome_label("home_run") == "Home run"
    assert outcome_label("strikeout") == "Strikeout"
    assert outcome_label("field_out") == "Field out"
    assert outcome_label("") == ""


# --- the pilot game, whole --------------------------------------------------


def test_every_home_run_in_the_pilot_game_is_selectable(pilot_events, pilot_roster):
    """The question that failed, answered by selection instead of by sample.

    Six home runs, five hitters, off the real feed. No prose is matched
    anywhere: the list comes from `event == "home_run"` alone.
    """
    cues = PlayByPlayRecognizer(pilot_events, 0.0, pilot_roster).recognize(MEDIA)
    homers = [c for c in cues if c.event == "home_run"]
    assert len(homers) == PILOT_HOME_RUNS
    hitters = {c.entity_ids[0] for c in homers}
    assert hitters == PILOT_HOME_RUN_HITTERS
    strikeouts = [c for c in cues if c.event == "strikeout"]
    assert len(strikeouts) == PILOT_STRIKEOUTS


def test_the_pilot_game_emits_one_outcome_per_at_bat(pilot_events, pilot_roster):
    """84 plays, 344 pitches: the outcomes must count the plays, not the
    pitches, or every long at-bat would be reported several times over."""
    cues = PlayByPlayRecognizer(pilot_events, 0.0, pilot_roster).recognize(MEDIA)
    counted = {"pitch": 0, "at_bat": 0}
    outcomes = 0
    for cue in cues:
        if cue.event in counted:
            counted[cue.event] += 1
        else:
            outcomes += 1
    assert counted == {"pitch": 344, "at_bat": 344}
    assert outcomes == 84


def test_the_phase_1_metric_does_not_move(pilot_events, pilot_roster):
    """The pitcher-keyed metric reads `event == "pitch"` alone, so it cannot
    see the outcome cues. Measured here on the whole pilot game rather than
    argued: the scorecards are equal, term by term."""
    alignment = estimate([Anchor(pilot_events[0].epoch_s, 60.0)])
    cues = PlayByPlayRecognizer(
        pilot_events, alignment.offset_s, pilot_roster
    ).recognize(MEDIA)
    before = [c for c in cues if c.event in ("pitch", "at_bat")]
    assert len(cues) > len(before)  # the outcome cues are really there

    after_card = score(fuse(cues), pilot_events, alignment, pilot_roster)
    before_card = score(fuse(before), pilot_events, alignment, pilot_roster)

    assert after_card.total_events == before_card.total_events == 344
    assert vars(after_card.overall) == vars(before_card.overall)
    assert after_card.per_source_found == before_card.per_source_found
    assert dict(after_card.per_player) == dict(before_card.per_player)
    # The two misses are two pitches thrown inside the fusion merge gap, which
    # this change does not touch: the same two miss before it. What matters is
    # that the number is the same one, and that no outcome cue became a false
    # positive.
    assert after_card.overall.tp == 342 and after_card.overall.fp == 0
