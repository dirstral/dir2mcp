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
            if pitcher is not None:
                # A pitch thrown BY a rostered player — the event the phase-1
                # metric scores (recall/precision are pitcher-keyed).
                cues.append(
                    Cue(
                        source=self.name,
                        start_s=start,
                        end_s=end,
                        event="pitch",
                        entity_ids=(pitcher.id,),
                        confidence=CONFIDENCE,
                        text=f"Pitch: {pitcher_name} to {batter_name}{where}{desc}",
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
                        entity_ids=(batter.id,),
                        confidence=CONFIDENCE,
                        text=f"At bat: {batter_name} vs {pitcher_name}{where}{desc}",
                    )
                )
        return cues
