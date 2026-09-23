"""dir2mcp #1025: the miss diagnosis explains the scorecard's own matching."""
from dirstral_annotator.eval import misses, score
from dirstral_annotator.eval.align import Alignment
from dirstral_annotator.eval.ground_truth import PitchEvent
from dirstral_annotator.model import Annotation
from dirstral_annotator.roster import Player, Roster


def _roster():
    return Roster([Player(id="player:lee-jordan", name="Jordan Lee", aliases=()),
                   Player(id="player:okafor-sam", name="Sam Okafor", aliases=())],
                  {1: "player:lee-jordan", 2: "player:okafor-sam"})


def _pitch(t, pitcher):
    return PitchEvent(game_pk=1, epoch_s=t, pitcher_id=pitcher, pitcher_name="", batter_id=9,
                      batter_name="", inning=1, description="Ball")


def _ann(start, end, pid, text="graphic"):
    return Annotation(start_s=start, end_s=end, event="pitch", entity_ids=(pid,), text=text,
                      confidence=0.9, sources=("scorebug",))


def test_causes_are_read_off_the_matching():
    roster = _roster()
    lee, oka = "player:lee-jordan", "player:okafor-sam"
    events = [
        _pitch(100, 1),   # credited by ann 0
        _pitch(104, 1),   # consumed: ann 0 spans both, scorer gave it to the first
        _pitch(200, 1),   # late: nearest own cue starts 12 s after
        _pitch(300, 1),   # misattributed: Okafor's cue covers it, none of Lee's near
        _pitch(400, 1),   # none
        _pitch(500, 2),   # credited by ann 3
    ]
    anns = [
        _ann(98, 106, lee, "Pitch 3 by Jordan Lee"),  # count cue spanning two pitches
        _ann(212, 214, lee),                           # late graphic
        _ann(298, 302, oka),                           # Okafor's cue on Lee's pitch: FP, invented for Okafor? no: near 500? 198 s away -> invented
        _ann(499, 501, oka),
        _ann(560, 562, oka, "Pitch 9 by Sam Okafor"),  # early/late FP: 60 s after 500 -> invented (beyond NEAR_S)
        _ann(503, 505, oka),                           # taken: pitch 500 within tolerance but credited to ann 3
    ]
    card = score.score(anns, events, Alignment(0.0, 0.0, 1), roster)
    assert (card.overall.tp, card.overall.fn, card.overall.fp) == (2, 4, 4)
    rep = misses.diagnose(card, Alignment(0.0, 0.0, 1), roster)
    causes = {m.t: m.cause for m in rep.misses}
    assert causes == {104: "consumed", 200: "late", 300: "misattributed", 400: "none"}
    assert {m.t: m.kind for m in rep.misses}[104] == "count"
    fps = {f.ann_index: f.cause for f in rep.false_positives}
    assert fps == {1: "late", 2: "invented", 4: "invented", 5: "taken"}
    assert rep.miss_causes()["consumed/count"] == 1 and rep.miss_causes()["none"] == 1
    assert rep.fp_causes()["taken/graphic"] == 1
    text = "\n".join(misses.render(card, Alignment(0.0, 0.0, 1), roster, rep))
    assert "| consumed/count | 1 |" in text and "| taken/graphic | 1 |" in text
    assert "Jordan Lee" in text and "| 0 min |" in text


def test_scorecard_matching_is_recorded():
    roster = _roster()
    events = [_pitch(10, 1)]
    anns = [_ann(9, 11, "player:lee-jordan")]
    card = score.score(anns, events, Alignment(0.0, 0.0, 1), roster)
    assert card.matched == {0: 0}
    assert len(card.scored) == 1 and len(card.pitch_anns) == 1


def test_diagnosis_uses_the_scorecard_tolerance_and_reports_its_search():
    roster = _roster()
    events = [_pitch(100, 1)]
    anns = [_ann(103, 104, "player:lee-jordan")]
    card = score.score(anns, events, Alignment(0.0, 0.0, 1), roster, tolerance_s=1.0)
    assert card.tolerance_s == 1.0 and card.overall.fn == 1
    rep = misses.diagnose(card, Alignment(0.0, 0.0, 1), roster, near_s=10.0)
    assert rep.tolerance_s == 1.0
    assert [m.cause for m in rep.misses] == ["late"], "a 1 s scorecard must not call this consumed"
    assert [f.cause for f in rep.false_positives] == ["late"]
    text = "\n".join(misses.render(card, Alignment(0.0, 0.0, 1), roster, rep))
    assert "±10s" in text and "±1.0s" in text
