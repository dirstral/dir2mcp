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

    def half_inning(self) -> str:
        """The half-inning phrased as a person would say it: "top of the 1st"."""
        if self.inning <= 0:
            return ""
        return f"{'top' if self.top_inning else 'bottom'} of the {_ordinal(self.inning)}"


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
    plays = feed.get("liveData", {}).get("plays", {}).get("allPlays", [])
    for play in plays:
        matchup = play.get("matchup", {})
        pitcher = matchup.get("pitcher", {})
        batter = matchup.get("batter", {})
        result_desc = play.get("result", {}).get("description", "")
        about = play.get("about", {})
        inning = about.get("inning", 0)
        top_inning = bool(about.get("isTopInning", True))
        for ev in play.get("playEvents", []):
            if not ev.get("isPitch"):
                continue
            start = ev.get("startTime")
            if not start:
                continue
            call = ev.get("details", {}).get("description", "")
            # The play's result text describes the final pitch of the at-bat
            # better than the raw call does.
            is_last = ev is play["playEvents"][-1]
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
                    description=(result_desc if is_last and result_desc else call),
                )
            )
    events.sort(key=lambda e: e.epoch_s)
    return events


def _iso_epoch(iso: str) -> float:
    return datetime.fromisoformat(iso.replace("Z", "+00:00")).timestamp()
