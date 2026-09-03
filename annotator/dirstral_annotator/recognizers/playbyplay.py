"""Play-by-play recognizer: inherit MLB's own labels.

Where a game has statsapi/Statcast coverage (roughly 2017+), the pitch-by-
pitch record with wall-clock timestamps is authoritative — no vision needed.
Given the game's `game_pk` and a wall-clock→video-time offset (estimated
from anchors, see eval.align), every recorded pitch becomes a high-
confidence cue on the video timeline.

A pitch has two rostered-relevant roles: the pitcher *threw* it and the
batter *faced* it. They are emitted as distinct events — `pitch` for the
pitcher, `at_bat` for the batter — because the phase-1 metric is pitcher-
keyed: a rostered batter appearing at an opponent's pitch is a real
appearance, not a pitch by that player, and tagging it `pitch` would make
every opponent-pitched at-bat a false positive.

The pitch that ENDS an at-bat carries a third cue: the outcome. Its `event`
is the feed's own `result.eventType` ("home_run", "strikeout", "walk",
"field_out", ...) and it is keyed on the batter, because the outcome is the
batter's. Before it, the outcome existed only as prose inside the cue text,
so `dir2mcp_search`'s `events` filter could not reach it: "who hit home
runs?" had to fall back to top-k semantic search over that prose, and a
"list every X" question answered from a partial sample invented one player
and dropped another. A structured event makes the same question a selection.

Four more cues say how notable the play was and how hard the ball was hit:
`captivating`, `reviewed`, `scoring_play` and `batted_ball`. They exist
because the outcome answers "who homered" and nothing else: "the most
captivating moments", "the hardest hit ball", "the longest home run" and "the
contested calls" had no structured handle at all, and the feed answers every
one of them.

A number is neither an event nor an entity, and the design 0004 wire schema
closes `annotations[]` (`additionalProperties: false`), so it has no numeric
field to hold one. Each fact therefore reaches a client by the two channels
the contract does have, each doing the job it can do:

  `event`  the token to FILTER on. One cue carries one event string, so N
           selectable facts need N cues. They share the ending pitch's span,
           so the answer path groups them into one moment (#784).
  `text`   the number itself, verbatim and in the feed's units, inside the
           sentence that gets indexed. It is what a client RANKS on and what
           an answer quotes.

`captivatingIndex` is a score, and this recognizer sets no threshold on it.
The one boundary the feed itself draws is zero: 43 of the pilot game's 84
plays score exactly 0. So `captivating` means "the feed scored this play
above zero", and the score rides in the text. A sharper cut (>= 33, >= 70)
would be this backend's judgement baked into the index, where no client could
undo it.

Every cue is additive. `pitch` and `at_bat` keep the entities, the text, the
span and the confidence they had, so the pitcher-keyed phase-1 metric
(`eval.score`, which reads `event == "pitch"` alone) cannot see this change
at all. Retagging is what broke #620; this adds instead.

This recognizer doubles as the eval ground-truth source; the fetch/parse
lives in eval.ground_truth so both uses share one implementation.
"""

from __future__ import annotations

import re
from pathlib import Path

from ..eval.ground_truth import HitData, PitchEvent
from ..model import Cue
from ..roster import Roster

# Statsapi timestamps are trustworthy but the broadcast cut lags/leads by a
# beat; pad the window so the cited clip contains the full delivery.
PRE_ROLL_S = 3.0
POST_ROLL_S = 5.0
CONFIDENCE = 0.98  # official record, minus alignment slop

# The two role events this recognizer owns. An outcome may never take one of
# these names: a batter-keyed cue tagged `pitch` is the #620 regression, and
# the pitcher-keyed metric would score it as a false positive. MLB types no
# play "pitch" or "at_bat", so the guard should never fire. It costs one
# comparison, and it means the metric does not depend on a third party's
# vocabulary staying the way it is today.
ROLE_EVENTS = ("pitch", "at_bat")

# The notability and measurement events, in the order they are emitted. They are
# fixed names this recognizer owns, not feed tokens, because the feed states
# these facts as flags and numbers rather than as a vocabulary. None of them may
# ever collide with ROLE_EVENTS, for the reason above; a test pins that.
BATTED_BALL_EVENT = "batted_ball"
CAPTIVATING_EVENT = "captivating"
REVIEWED_EVENT = "reviewed"
SCORING_EVENT = "scoring_play"
NOTABILITY_EVENTS = (BATTED_BALL_EVENT, CAPTIVATING_EVENT, REVIEWED_EVENT, SCORING_EVENT)

_SLUG_SEP = re.compile(r"[^a-z0-9]+")


def team_id(name: str) -> str:
    """"San Francisco Giants" -> "team:san-francisco-giants".

    The slug round-trips through the emit layer's label fallback
    (`id.split(":")[-1].replace("-", " ").title()`), so a club needs no roster
    entry to reach the wire with a readable label. Returns "" for a name that
    slugs to nothing, so callers can drop it rather than emit `team:`.
    """
    slug = _SLUG_SEP.sub("-", name.strip().lower()).strip("-")
    return f"team:{slug}" if slug else ""


def outcome_label(event_type: str) -> str:
    """"home_run" -> "Home run". The feed's own vocabulary, spelled for a
    reader, so the cue text stays a sentence a person can read while `event`
    stays the exact token a filter selects on.

    Returns "" for an event type that is empty or only whitespace, which is
    how a feed without `result.eventType` reaches here.
    """
    words = event_type.replace("_", " ").strip()
    return words[:1].upper() + words[1:]


def measurement_phrase(hit: HitData) -> str:
    """The batted ball's measurements as a clause a person reads.

    "exit velocity 107.9 mph, launch angle 5 degrees, distance 119 ft, ground
    ball, hard contact". Every number is the feed's, in the feed's unit, and
    nothing is rounded: the trailing ".0" of a whole measurement is dropped and
    that is all. This clause is the only place a number can reach a client,
    because the wire schema closes `annotations[]` and has no numeric field, so
    it is also what a client ranks "the hardest hit ball" on.

    A measurement the tracking did not take is left out rather than printed as a
    zero. Returns "" when it took none, and the caller then emits no cue.
    """
    parts = []
    if hit.launch_speed is not None:
        parts.append(f"exit velocity {_number(hit.launch_speed)} mph")
    if hit.launch_angle is not None:
        parts.append(f"launch angle {_number(hit.launch_angle)} degrees")
    if hit.total_distance is not None:
        parts.append(f"distance {_number(hit.total_distance)} ft")
    if hit.trajectory:
        parts.append(hit.trajectory.replace("_", " "))
    if hit.hardness:
        parts.append(f"{hit.hardness} contact")
    return ", ".join(parts)


def _number(value: float) -> str:
    """107.0 -> "107", 107.9 -> "107.9". The value, not a rounding of it."""
    return str(int(value)) if float(value).is_integer() else str(value)


def _inning_attributes(ev: PitchEvent) -> dict[str, str]:
    """The inning as a REQUIRABLE scope (SPEC §9.10), not just prose.

    The half-inning already rides in the cue text ("bottom of the 8th"), which
    lets an inning query be PREFERRED by similarity — and rank a talkative 7th
    above a quiet 8th (#928 measured exactly that). The attributes are the
    structured form a filter can require: `{"inning": ["8"]}` on search/ask
    matches these values byte-for-byte.

    Canonical forms, fixed here and compared verbatim by the server: the
    inning as an unpadded decimal ("8", "10"), the half as lowercase "top" or
    "bottom". An event without an inning (feed gap) states no scope at all
    rather than a made-up one.
    """
    if ev.inning <= 0:
        return {}
    return {"inning": str(ev.inning), "half": "top" if ev.top_inning else "bottom"}


def _with_team(player_id: str, team: str) -> tuple[str, ...]:
    """The acting player, plus the club they are acting for.

    The club rides in `entity_ids` and deliberately NOT in the cue text.
    Writing it into the text was measured on the pilot corpus and made
    retrieval WORSE: every statement names both clubs, so the label cannot
    discriminate between candidates, and it drags a team-scoped query onto
    whichever role happens to rank first. "Giants home run" came back as a
    Giants pitcher throwing balls (dirstral-spec design 0004 §6.1).

    As an entity it is exact, because the cue's `event` already records which
    role this id is acting in: `pitch` is keyed on the pitcher and carries the
    fielding club, `at_bat` is keyed on the batter and carries the club at the
    plate. Selecting on entity AND event is therefore role-correct, which no
    amount of phrasing in the text can achieve.
    """
    tid = team_id(team)
    return (player_id, tid) if tid else (player_id,)


class PlayByPlayRecognizer:
    name = "playbyplay"

    def __init__(self, events: list[PitchEvent], offset_s: float, roster: Roster):
        """`offset_s` converts event epoch seconds to video seconds:
        video_t = event.epoch_s + offset_s."""
        self.events = events
        self.offset_s = offset_s
        self.roster = roster

    def recognize(self, media_path: Path) -> list[Cue]:  # media unused: labels are external
        cues = []
        for ev in self.events:
            video_t = ev.epoch_s + self.offset_s
            start = max(0.0, video_t - PRE_ROLL_S)
            end = video_t + POST_ROLL_S
            if end < start:
                continue  # event predates the video entirely; nothing to cite
            pitcher = self.roster.by_mlbam(ev.pitcher_id)
            batter = self.roster.by_mlbam(ev.batter_id)
            if pitcher is None and batter is None:
                continue  # nobody on our roster is involved; skip, don't guess
            pitcher_name = pitcher.name if pitcher else ev.pitcher_name
            batter_name = batter.name if batter else ev.batter_name
            # The inning is what makes a moment findable by the question a
            # person actually asks ("bottom of the 9th", "in the 7th"). It was
            # parsed from the feed and then dropped before the text, so no
            # inning query could match anything (#741).
            half = ev.half_inning()
            where = f" ({half})" if half else ""
            desc = f" — {ev.description}" if ev.description else ""
            # The inning ALSO rides as structured attributes (SPEC §9.10), on
            # every cue of the event: the prose above lets an inning be
            # preferred by similarity, the attributes let it be REQUIRED
            # ({"attributes": {"inning": ["8"]}} on search/ask). Values are the
            # canonical forms dir2mcp will compare byte-for-byte: the inning
            # unpadded ("8", "10"), the half lowercase "top"/"bottom".
            attrs = _inning_attributes(ev)
            if pitcher is not None:
                # A pitch thrown BY a rostered player — the event the phase-1
                # metric scores (recall/precision are pitcher-keyed).
                cues.append(
                    Cue(
                        source=self.name,
                        start_s=start,
                        end_s=end,
                        event="pitch",
                        entity_ids=_with_team(pitcher.id, ev.pitching_team()),
                        confidence=CONFIDENCE,
                        text=f"Pitch: {pitcher_name} to {batter_name}{where}{desc}",
                        attributes=dict(attrs),
                    )
                )
            if batter is not None:
                # A rostered player AT THE PLATE. They appear at this pitch but
                # did not throw it, so it is an at-bat, not a pitch — tagging it
                # "pitch" would count every opponent-pitched at-bat as a false
                # positive under the pitcher-keyed precision metric.
                cues.append(
                    Cue(
                        source=self.name,
                        start_s=start,
                        end_s=end,
                        event="at_bat",
                        entity_ids=_with_team(batter.id, ev.batting_team()),
                        confidence=CONFIDENCE,
                        text=f"At bat: {batter_name} vs {pitcher_name}{where}{desc}",
                        attributes=dict(attrs),
                    )
                )
            outcome = ev.event_type.strip()
            if batter is not None and outcome and outcome not in ROLE_EVENTS:
                # THE OUTCOME OF THE AT-BAT, as structured data. The feed types
                # every play ("home_run", "strikeout", "walk", ...), and only
                # the pitch that ended the at-bat carries it, so one at-bat
                # produces exactly one outcome cue.
                #
                # It is keyed on the batter and their club, like the `at_bat`
                # cue it accompanies: the batter is who homered. A rostered
                # pitcher facing an unrostered batter therefore gets no outcome
                # cue, because the cue would have nobody to name.
                #
                # The span is the span of that last pitch, which the answer
                # path groups with the `pitch` and `at_bat` annotations of the
                # same seconds (issue #784), so one moment stays one moment.
                cues.append(
                    Cue(
                        source=self.name,
                        start_s=start,
                        end_s=end,
                        event=outcome,
                        entity_ids=_with_team(batter.id, ev.batting_team()),
                        confidence=CONFIDENCE,
                        text=(
                            f"{outcome_label(outcome)}: {batter_name} "
                            f"vs {pitcher_name}{where}{desc}"
                        ),
                        attributes=dict(attrs),
                    )
                )
            if batter is not None:
                # HOW NOTABLE THE PLAY WAS, and how hard the ball was hit. Same
                # actor, same span, one cue per selectable fact.
                cues.extend(
                    self._notability_cues(
                        ev,
                        batter.id,
                        who=f"{batter_name} vs {pitcher_name}{where}",
                        desc=desc,
                        start=start,
                        end=end,
                    )
                )
        return cues

    def _notability_cues(
        self,
        ev: PitchEvent,
        batter_id: str,
        *,
        who: str,
        desc: str,
        start: float,
        end: float,
    ) -> list[Cue]:
        """How notable the play was, and how hard the ball was hit.

        One cue per fact, because a cue carries one `event` string and each of
        these facts has to be selectable on its own. Every cue is keyed on the
        batter and their club, like the outcome cue it accompanies: these are
        statements about the play, and the play's actor is the batter. A rostered
        pitcher facing an unrostered batter therefore gets none of them, because
        they would have nobody to name.

        They share the span of the pitch that ended the at-bat, which is the span
        of the `pitch`, `at_bat` and outcome cues of the same seconds, so the
        answer path groups the whole set into one moment (#784).
        """
        keyed = _with_team(batter_id, ev.batting_team())

        attrs = _inning_attributes(ev)

        def cue(event: str, text: str) -> Cue:
            return Cue(
                source=self.name,
                start_s=start,
                end_s=end,
                event=event,
                entity_ids=keyed,
                confidence=CONFIDENCE,
                text=text,
                attributes=dict(attrs),
            )

        out = []
        if ev.hit_data is not None:
            # A measured batted ball. This is the cue "the hardest hit ball" and
            # "the longest home run" are answered from, so it carries the play
            # result text as well: one chunk then holds both the number and what
            # the ball became, and no join across cues is needed.
            phrase = measurement_phrase(ev.hit_data)
            if phrase:
                out.append(cue(BATTED_BALL_EVENT, f"Batted ball: {who}: {phrase}{desc}"))
        if ev.captivating_index > 0:
            # Above zero is the feed's own boundary and the only one it draws;
            # the score itself rides in the text, so the ranking stays MLB's.
            out.append(
                cue(
                    CAPTIVATING_EVENT,
                    f"Captivating moment (captivating index "
                    f"{ev.captivating_index}): {who}{desc}",
                )
            )
        if ev.has_review:
            out.append(cue(REVIEWED_EVENT, f"Reviewed call: {who}{desc}"))
        if ev.is_scoring_play:
            # The score AFTER the play, by half rather than by club name: a club
            # in the text is the measured retrieval regression (§6.1 above). A
            # run can score with no RBI, and "0 RBI" would read as a claim about
            # the batter that the feed did not make, so that clause is dropped.
            runs = f"{ev.rbi} RBI, " if ev.rbi > 0 else ""
            out.append(
                cue(
                    SCORING_EVENT,
                    f"Scoring play ({runs}score: away {ev.away_score}, "
                    f"home {ev.home_score}): {who}{desc}",
                )
            )
        return out
