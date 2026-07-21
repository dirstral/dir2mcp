"""Play-by-play recognizer: inherit MLB's own labels.

Where a game has statsapi/Statcast coverage (roughly 2017+), the pitch-by-
pitch record with wall-clock timestamps is authoritative — no vision needed.
Given the game's `game_pk` and a wall-clock→video-time offset (estimated
from anchors, see eval.align), every recorded pitch becomes a high-
confidence cue on the video timeline.

This recognizer doubles as the eval ground-truth source; the fetch/parse
lives in eval.ground_truth so both uses share one implementation.
"""

from __future__ import annotations

from pathlib import Path

from ..eval.ground_truth import PitchEvent
from ..model import Cue
from ..roster import Roster

# Statsapi timestamps are trustworthy but the broadcast cut lags/leads by a
# beat; pad the window so the cited clip contains the full delivery.
PRE_ROLL_S = 3.0
POST_ROLL_S = 5.0
CONFIDENCE = 0.98  # official record, minus alignment slop


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
            pitcher = self.roster.by_mlbam(ev.pitcher_id)
            batter = self.roster.by_mlbam(ev.batter_id)
            entities = tuple(p.id for p in (pitcher, batter) if p)
            if not entities:
                continue  # nobody on our roster is involved; skip, don't guess
            pitcher_name = pitcher.name if pitcher else ev.pitcher_name
            batter_name = batter.name if batter else ev.batter_name
            desc = f" — {ev.description}" if ev.description else ""
            cues.append(
                Cue(
                    source=self.name,
                    start_s=start,
                    end_s=end,
                    event="pitch",
                    entity_ids=entities,
                    confidence=CONFIDENCE,
                    text=f"Pitch: {pitcher_name} to {batter_name}{desc}",
                )
            )
        return cues
