"""Ground truth from MLB statsapi's GUMBO live feed.

`https://statsapi.mlb.com/api/v1.1/game/{game_pk}/feed/live` is public, free
and keyless, and records every pitch with pitcher/batter identity and ISO
wall-clock timestamps — the labels the pilot evaluates against (and that the
play-by-play recognizer inherits) with zero manual annotation.
"""

from __future__ import annotations

import json
import urllib.request
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

STATSAPI_URL = "https://statsapi.mlb.com/api/v1.1/game/{game_pk}/feed/live"


def _ordinal(n: int) -> str:
    """1 -> "1st". Baseball innings, so the teens case still has to be right."""
    if 10 <= n % 100 <= 20:
        suffix = "th"
    else:
        suffix = {1: "st", 2: "nd", 3: "rd"}.get(n % 10, "th")
    return f"{n}{suffix}"


@dataclass(frozen=True)
class HitData:
    """What the tracking system measured on a batted ball.

    Every field is `playEvents[].hitData`, verbatim, in the units the feed uses
    (mph, degrees, feet). Nothing here is derived, scaled or bucketed: "the
    hardest hit ball" and "the longest home run" have to be answerable against
    MLB's own measurement, not against a number this backend produced.

    A field the tracking did not measure stays absent (None, or "") rather than
    becoming a zero, because 0 mph is a claim and "not measured" is not.
    """

    launch_speed: float | None = None  # exit velocity, mph
    launch_angle: float | None = None  # degrees off the bat
    total_distance: float | None = None  # feet travelled
    trajectory: str = ""  # "fly_ball" | "ground_ball" | "line_drive" | "popup"
    hardness: str = ""  # "soft" | "medium" | "hard"

    def measured(self) -> bool:
        """True when the feed measured anything at all.

        A `hitData` object with every field empty says nothing a reader or a
        ranking can use, so callers drop it instead of reporting a batted ball
        with no measurement in it.
        """
        return any(
            (
                self.launch_speed is not None,
                self.launch_angle is not None,
                self.total_distance is not None,
                self.trajectory,
                self.hardness,
            )
        )


@dataclass(frozen=True)
class PitchEvent:
    game_pk: int
    epoch_s: float  # wall-clock UTC epoch seconds of the pitch
    pitcher_id: int  # MLBAM ids
    pitcher_name: str
    batter_id: int
    batter_name: str
    inning: int
    description: str  # outcome text, e.g. "In play, out(s)" / play result
    #: True for the top half. Carried because "bottom of the 9th" is how people
    #: ask for a moment, and an inning number alone cannot answer it.
    top_inning: bool = True
    #: Club names as the feed spells them. Empty when the payload omits them,
    #: which keeps every existing caller and fixture valid.
    away_team: str = ""
    home_team: str = ""
    #: The at-bat's outcome as the feed types it: `result.eventType`, verbatim
    #: ("home_run", "strikeout", "walk", "field_out", ...). It rides on the
    #: pitch that ENDED the at-bat and is empty on every other pitch, exactly
    #: like `description`, which already carries the play result text only
    #: there. Empty as well when the payload omits the field.
    #:
    #: The outcome is the one part of a play that used to exist only as prose
    #: inside `description`. A question like "who hit home runs?" then had no
    #: structured field to select on, so it fell back to top-k search over that
    #: prose and answered a "list every X" question from a partial sample.
    event_type: str = ""
    #: What the tracking measured on THIS pitch (`playEvents[].hitData`), or
    #: None for a pitch that was not hit. It is per pitch, not per play, so it
    #: rides on its own pitch: a foul that Statcast measured is a measurement of
    #: that foul, not of the at-bat's result.
    hit_data: HitData | None = None
    #: How notable the play was, on MLB's own `about.captivatingIndex`,
    #: verbatim. It is the answer to "the best moments of the game" taken from
    #: the source instead of from a model's opinion. Measured on the pilot game:
    #: 43 of the 84 plays score exactly 0 and one scores 95, so it does
    #: discriminate.
    #:
    #: Like every other play-level field below, it rides on the pitch that ENDED
    #: the at-bat, exactly where `event_type` and the play result text ride. One
    #: play then produces one notable moment, not one per pitch.
    captivating_index: int = 0
    #: `about.hasReview`: the call went to a review. This is "the contested
    #: calls", and it is 2 plays of the pilot game's 84.
    has_review: bool = False
    #: `about.isScoringPlay`: a run scored (15 plays of the pilot game).
    is_scoring_play: bool = False
    #: `result.rbi`, and the score AFTER the play (`result.awayScore` /
    #: `result.homeScore`). A run can score with no RBI (an error, a wild
    #: pitch), so the two are carried separately rather than derived from one
    #: another.
    rbi: int = 0
    away_score: int = 0
    home_score: int = 0

    def half_inning(self) -> str:
        """The half-inning phrased as a person would say it: "top of the 1st"."""
        if self.inning <= 0:
            return ""
        return f"{'top' if self.top_inning else 'bottom'} of the {_ordinal(self.inning)}"

    def batting_team(self) -> str:
        """The club at the plate. The visitors bat in the top half."""
        return self.away_team if self.top_inning else self.home_team

    def pitching_team(self) -> str:
        """The club in the field."""
        return self.home_team if self.top_inning else self.away_team


def fetch_game(game_pk: int, timeout_s: float = 30.0) -> dict:
    req = urllib.request.Request(
        STATSAPI_URL.format(game_pk=game_pk),
        headers={"User-Agent": "dirstral-annotator/0.1 (pilot eval)"},
    )
    with urllib.request.urlopen(req, timeout=timeout_s) as resp:
        return json.load(resp)


def load_game(path: str | Path) -> dict:
    """Load a previously saved GUMBO payload (tests, offline reruns)."""
    return json.loads(Path(path).read_text(encoding="utf-8"))


def parse_pitches(feed: dict) -> list[PitchEvent]:
    game_pk = feed.get("gamePk", 0)
    events: list[PitchEvent] = []
    # Which club bats follows from the half-inning, so the two club names are
    # all that is needed; no per-player team lookup.
    feed_teams = feed.get("gameData", {}).get("teams", {})
    away_team = (feed_teams.get("away", {}) or {}).get("name", "")
    home_team = (feed_teams.get("home", {}) or {}).get("name", "")
    plays = feed.get("liveData", {}).get("plays", {}).get("allPlays", [])
    for play in plays:
        matchup = play.get("matchup", {})
        pitcher = matchup.get("pitcher", {})
        batter = matchup.get("batter", {})
        result = play.get("result", {})
        result_desc = result.get("description", "")
        event_type = (result.get("eventType") or "").strip()
        about = play.get("about", {})
        inning = about.get("inning", 0)
        top_inning = bool(about.get("isTopInning", True))
        # Play-level notability, verbatim. It rides on the pitch that ended the
        # at-bat, so one play reports one notable moment.
        captivating_index = _int(about.get("captivatingIndex"))
        has_review = bool(about.get("hasReview"))
        is_scoring_play = bool(about.get("isScoringPlay"))
        rbi = _int(result.get("rbi"))
        away_score = _int(result.get("awayScore"))
        home_score = _int(result.get("homeScore"))
        play_events = play.get("playEvents", [])
        last = _last_recorded_pitch(play_events)
        for i, ev in enumerate(play_events):
            if not ev.get("isPitch"):
                continue
            start = ev.get("startTime")
            if not start:
                continue
            call = ev.get("details", {}).get("description", "")
            # The play's result text describes the final pitch of the at-bat
            # better than the raw call does.
            is_last = i == last
            events.append(
                PitchEvent(
                    game_pk=game_pk,
                    epoch_s=_iso_epoch(start),
                    pitcher_id=int(pitcher.get("id", 0)),
                    pitcher_name=pitcher.get("fullName", ""),
                    batter_id=int(batter.get("id", 0)),
                    batter_name=batter.get("fullName", ""),
                    inning=inning,
                    top_inning=top_inning,
                    away_team=away_team,
                    home_team=home_team,
                    description=(result_desc if is_last and result_desc else call),
                    event_type=(event_type if is_last else ""),
                    hit_data=_hit_data(ev.get("hitData")),
                    captivating_index=(captivating_index if is_last else 0),
                    has_review=(has_review and is_last),
                    is_scoring_play=(is_scoring_play and is_last),
                    rbi=(rbi if is_last else 0),
                    away_score=(away_score if is_last else 0),
                    home_score=(home_score if is_last else 0),
                )
            )
    events.sort(key=lambda e: e.epoch_s)
    return events


def _last_recorded_pitch(play_events: list[dict]) -> int:
    """Index of the pitch that ends the at-bat, or -1 for a play with none.

    A play does not always end on a pitch, and the test this replaced
    (`ev is play["playEvents"][-1]`) ran AFTER the loop had skipped every
    non-pitch, so a play that ends on anything else marked no pitch as last.
    The play result text, and now the outcome, then attached to nothing.

    Measured on 594 plays of 8 games of the 2025 season: one play ended on an
    "Automatic Ball - Pitcher Pitch Timer Violation", which the feed records
    with `isPitch: false`. It is rare, not theoretical, and one dropped
    outcome is one at-bat that no structured query can reach.

    A pitch with no `startTime` cannot be placed on the video timeline, so the
    parse skips it; it cannot end an at-bat either, or the outcome would ride
    on a pitch that never reaches a caller.
    """
    for i in range(len(play_events) - 1, -1, -1):
        ev = play_events[i]
        if ev.get("isPitch") and ev.get("startTime"):
            return i
    return -1


def _hit_data(raw: object) -> HitData | None:
    """`playEvents[].hitData` as a HitData, or None when nothing was measured.

    A pitch that was not hit carries no `hitData` at all, and a `hitData` whose
    every field is empty measures nothing, so both read as None: a batted ball
    with no measurement in it is a claim the feed did not make.
    """
    if not isinstance(raw, dict):
        return None
    hit = HitData(
        launch_speed=_float(raw.get("launchSpeed")),
        launch_angle=_float(raw.get("launchAngle")),
        total_distance=_float(raw.get("totalDistance")),
        trajectory=_text(raw.get("trajectory")),
        hardness=_text(raw.get("hardness")),
    )
    return hit if hit.measured() else None


def _int(value: object) -> int:
    """A whole number off the feed, or 0 for anything that is not one.

    A bool is not a measurement (`True` would read as 1), and a missing field
    reads as 0, which is the value that means "nothing to report" for every
    caller of these fields.
    """
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value) if value == value and abs(value) != float("inf") else 0
    if isinstance(value, str):
        try:
            return int(value.strip())
        except ValueError:
            return 0
    return 0


def _float(value: object) -> float | None:
    """A measurement off the feed, or None when there is not one.

    None rather than 0.0, because 0.0 mph is a measurement and an absent field
    is not. NaN and infinity are dropped as well: they cannot be ranked and they
    cannot be read.
    """
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        number = float(value)
    elif isinstance(value, str):
        try:
            number = float(value.strip())
        except ValueError:
            return None
    else:
        return None
    if number != number or abs(number) == float("inf"):
        return None
    return number


def _text(value: object) -> str:
    """A label off the feed ("fly_ball", "hard"), or "" for anything else."""
    return value.strip() if isinstance(value, str) else ""


def _iso_epoch(iso: str) -> float:
    return datetime.fromisoformat(iso.replace("Z", "+00:00")).timestamp()
