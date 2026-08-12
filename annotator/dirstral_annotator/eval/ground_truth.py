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


def _iso_epoch(iso: str) -> float:
    return datetime.fromisoformat(iso.replace("Z", "+00:00")).timestamp()
