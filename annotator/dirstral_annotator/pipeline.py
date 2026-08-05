"""Recognition pipeline: media file -> fused annotations.

Shared by the serve handler (per-request) and the eval CLI. Vision
recognizers are constructed lazily per pipeline; ones whose backends are
missing are skipped with a note, so the cascade degrades instead of
aborting (a metadata-only deployment still recognizes via play-by-play).
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

from .eval import align, ground_truth
from .fusion import fuse
from .model import Annotation, Cue
from .recognizers.base import RecognizerUnavailable
from .roster import Roster


@dataclass
class GameConfig:
    """Per-media play-by-play binding: where the official labels come from
    and how their wall clock maps onto this video's timeline."""

    anchors: list[align.Anchor]
    game_pk: int | None = None
    feed: str | None = None  # saved GUMBO JSON path (wins over game_pk)

    @classmethod
    def parse(cls, raw: dict) -> "GameConfig":
        anchors = []
        for spec in raw.get("anchors", []):
            epoch, video = str(spec).split("=", 1)
            anchors.append(align.Anchor(epoch_s=float(epoch), video_s=float(video)))
        return cls(anchors=anchors, game_pk=raw.get("game_pk"), feed=raw.get("feed"))

    def events(self) -> list[ground_truth.PitchEvent]:
        feed = ground_truth.load_game(self.feed) if self.feed else ground_truth.fetch_game(self.game_pk)
        return ground_truth.parse_pitches(feed)

    def alignment(self) -> align.Alignment:
        return align.estimate(self.anchors)


def load_games(path: str | Path) -> dict[str, GameConfig]:
    """games.json: {"<media basename>": {"game_pk"|"feed": ..., "anchors": ["EPOCH=VIDEO_S", ...]}}"""
    raw = json.loads(Path(path).read_text(encoding="utf-8"))
    return {name: GameConfig.parse(cfg) for name, cfg in raw.items()}


@dataclass
class Pipeline:
    roster: Roster
    games: dict[str, GameConfig] = field(default_factory=dict)
    scorebug: bool = False
    jersey: bool = False
    news: bool = False
    ocr_lang: str | None = None
    faces_bank: Path | None = None
    fps: float = 0.5
    min_confidence: float = 0.0
    skipped: list[str] = field(default_factory=list)

    def annotations_for(self, media_path: Path) -> list[Annotation]:
        return fuse(self.cues_for(media_path), min_confidence=self.min_confidence)

    def cues_for(self, media_path: Path) -> list[Cue]:
        cues: list[Cue] = []
        self.skipped = []

        game = self.games.get(media_path.name)
        if game is not None and game.anchors and (game.game_pk or game.feed):
            from .recognizers.playbyplay import PlayByPlayRecognizer

            alignment = game.alignment()
            cues += PlayByPlayRecognizer(
                game.events(), alignment.offset_s, self.roster
            ).recognize(media_path)

        def try_recognizer(build):
            try:
                cues.extend(build().recognize(media_path))
            except RecognizerUnavailable as exc:
                self.skipped.append(str(exc))

        if self.scorebug:
            from .recognizers.scorebug import ScorebugRecognizer

            try_recognizer(lambda: ScorebugRecognizer(self.roster, fps=self.fps))
        if self.jersey:
            from .recognizers.jersey import JerseyRecognizer

            try_recognizer(lambda: JerseyRecognizer(self.roster, fps=self.fps))
        if self.faces_bank:
            from .recognizers.faces import FaceRecognizer

            try_recognizer(lambda: FaceRecognizer(self.roster, self.faces_bank, fps=self.fps))
        if self.news:
            # The only recognizer here that reads no roster: overlay text is
            # the payload, not a name to resolve.
            from .recognizers.news import NewsOverlayRecognizer

            try_recognizer(
                lambda: NewsOverlayRecognizer(lang=self.ocr_lang, fps=self.fps)
            )
        return cues
