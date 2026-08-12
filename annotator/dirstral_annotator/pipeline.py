"""Recognition pipeline: media file -> fused annotations.

Shared by the serve handler and the eval CLI. Vision recognizers are
constructed lazily and then CACHED for the pipeline's life; ones whose backends
are missing are skipped with a note, so the cascade degrades instead of
aborting (a metadata-only deployment still recognizes via play-by-play).

"Lazily" means "not at process start", not "per request". `serve` keeps one
pipeline behind a ThreadingHTTPServer, so building inside `cues_for` reloaded
the ONNX and YOLO models and re-embedded the whole face bank on every POST:
measured at 46s per request for a 40 image bank on the real InsightFace backend
(#650). Caching also removes an inconsistency, because a bank edited during a
run was picked up at whichever request happened to re-enroll.

A cached recognizer is shared by concurrent requests, and these recognizers are
not reentrant, so one request at a time enters any one of them. See `_Shared`.
"""

from __future__ import annotations

import json
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

from .eval import align, ground_truth
from .fusion import fuse
from .model import Annotation, Cue
from .recognizers.base import Recognizer, RecognizerUnavailable
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


@dataclass(frozen=True)
class _Unavailable:
    """A recognizer whose backend is missing, remembered rather than retried.

    The reason is replayed on every request, so a caller reading `skipped` gets
    the same answer each time. Only the probing is done once.
    """

    reason: str


class _Shared:
    """One recognizer instance, entered by one request at a time.

    Caching an instance means concurrent requests share it. That is the point:
    building per request reloaded the models, and building per thread would
    duplicate the allocations rather than remove them.

    Sharing needs serialised inference, because these recognizers are not
    reentrant. `NewsOverlayRecognizer.recognize` clears and refills per-role
    reader state on itself for each run, so two runs at once interleave their
    sightings. The default jersey detector is an ultralytics model, which is not
    documented thread safe (see `_Detectors`). insightface hangs per-call state
    off its app object.

    So the contract is one lock per recognizer: two requests queue instead of
    corrupting each other. They still share the frame extraction cache in
    `recognizers.base`, which is where a real request's time goes, so the second
    request does not re-decode the media either.
    """

    def __init__(self, recognizer: Recognizer):
        self.recognizer = recognizer
        self._lock = threading.Lock()

    def recognize(self, media_path: Path) -> list[Cue]:
        with self._lock:
            return self.recognizer.recognize(media_path)


def load_games(path: str | Path) -> dict[str, GameConfig]:
    """games.json: {"<media basename>": {"game_pk"|"feed": ..., "anchors": ["EPOCH=VIDEO_S", ...]}}"""
    raw = json.loads(Path(path).read_text(encoding="utf-8"))
    return {name: GameConfig.parse(cfg) for name, cfg in raw.items()}


@dataclass
class Pipeline:
    roster: Roster
    games: dict[str, GameConfig] = field(default_factory=dict)
    scorebug: bool = False
    #: Opt-in: also read a pitch from each +1 transition of the scorebug's
    #: pitch count. Trades precision for recall (see ScorebugRecognizer), so
    #: it stays off unless an operator asks for it. Inert without `scorebug`.
    scorebug_pitch_counts: bool = False
    jersey: bool = False
    news: bool = False
    #: Readability gate on the news overlay cues. None keeps the recognizer's
    #: measured defaults, which are ON: see `READABLE_MIN_CHARS`. An operator
    #: who wants the ungated stream passes 0 to both.
    news_min_chars: int | None = None
    news_min_agreement: float | None = None
    ocr_lang: str | None = None
    faces_bank: Path | None = None
    fps: float = 0.5
    min_confidence: float = 0.0

    def __post_init__(self) -> None:
        # Not dataclass fields on purpose. This is internal state, a Lock is
        # neither comparable nor copyable, and a loaded model has no place in a
        # repr of the configuration.
        self._build_lock = threading.Lock()
        self._recognizers: dict[str, tuple[tuple, _Shared | _Unavailable]] = {}
        # Skip notes belong to one request, and `serve` runs a request per
        # thread. A plain attribute is shared, so a second request replaces the
        # first request's notes before its caller has read them: no list is
        # interleaved, and the caller still gets the wrong answer. Thread-local
        # storage gives each request its own.
        self._notes = threading.local()

    @property
    def skipped(self) -> list[str]:
        """Skip notes from the most recent `cues_for` ON THIS THREAD.

        Read after the call that produced them, which is what the eval CLI and a
        request handler both do.
        """
        return getattr(self._notes, "skipped", [])

    def _shared(
        self, name: str, key: tuple, build: Callable[[], Recognizer]
    ) -> _Shared | _Unavailable:
        """The configured recognizer for `name`, built at most once.

        `key` is everything about this pipeline that decides WHAT gets built.
        One slot per name, replaced when its key changes, so an operator who
        repoints `faces_bank` or changes `fps` gets a recognizer built from the
        new setting instead of the one the old setting produced, and a remembered
        unavailability belongs to one configuration rather than to the process.

        Construction holds the lock, and the lock is one per pipeline rather than
        one per name. That is deliberate: two concurrent first requests must not
        both load the models, and neither should a face build and a jersey build
        want their weights at the same moment. It costs a cold second request its
        wait, which it would spend queueing on the recognizer anyway, and a warm
        request only a dict lookup.
        """
        with self._build_lock:
            slot = self._recognizers.get(name)
            if slot is not None and slot[0] == key:
                return slot[1]
            built: _Shared | _Unavailable
            try:
                built = _Shared(build())
            except RecognizerUnavailable as exc:
                built = _Unavailable(str(exc))
            self._recognizers[name] = (key, built)
            return built

    def annotations_for(self, media_path: Path) -> list[Annotation]:
        return fuse(self.cues_for(media_path), min_confidence=self.min_confidence)

    def cues_for(self, media_path: Path) -> list[Cue]:
        cues: list[Cue] = []
        # Appended to through the local name and published to this thread only,
        # so two concurrent requests neither interleave into one list nor
        # overwrite each other's answer. See the `skipped` property.
        skipped: list[str] = []
        self._notes.skipped = skipped

        game = self.games.get(media_path.name)
        if game is not None and game.anchors and (game.game_pk or game.feed):
            from .recognizers.playbyplay import PlayByPlayRecognizer

            alignment = game.alignment()
            cues += PlayByPlayRecognizer(
                game.events(), alignment.offset_s, self.roster
            ).recognize(media_path)

        def try_recognizer(name, key, build):
            entry = self._shared(name, key, build)
            if isinstance(entry, _Unavailable):
                skipped.append(entry.reason)
                return
            try:
                cues.extend(entry.recognize(media_path))
            except RecognizerUnavailable as exc:
                # A backend that only fails on its first READ, as the OCR
                # adapter does. That is not a construction failure, so it is
                # reported per request and the instance is not written off: the
                # engine can be installed under a long lived server.
                skipped.append(str(exc))

        if self.scorebug:
            from .recognizers.scorebug import ScorebugRecognizer

            try_recognizer(
                "scorebug",
                (self.roster, self.fps, self.scorebug_pitch_counts),
                lambda: ScorebugRecognizer(
                    self.roster, fps=self.fps,
                    count_pitch_cues=self.scorebug_pitch_counts,
                ),
            )
        if self.jersey:
            from .recognizers.jersey import JerseyRecognizer

            try_recognizer(
                "jersey",
                (self.roster, self.fps),
                lambda: JerseyRecognizer(self.roster, fps=self.fps),
            )
        if self.faces_bank:
            from .recognizers.faces import FaceRecognizer

            try_recognizer(
                "face",
                (self.roster, self.faces_bank, self.fps),
                lambda: FaceRecognizer(self.roster, self.faces_bank, fps=self.fps),
            )
        if self.news:
            # The only recognizer here that reads no roster: overlay text is
            # the payload, not a name to resolve.
            from .recognizers.news import NewsOverlayRecognizer

            # Only the floors an operator actually set are forwarded, so the
            # measured defaults stay in one place: the recognizer.
            gate = {}
            if self.news_min_chars is not None:
                gate["readable_chars"] = self.news_min_chars
            if self.news_min_agreement is not None:
                gate["readable_agreement"] = self.news_min_agreement
            try_recognizer(
                "news",
                (self.ocr_lang, self.fps, self.news_min_chars,
                 self.news_min_agreement),
                lambda: NewsOverlayRecognizer(
                    lang=self.ocr_lang, fps=self.fps, **gate
                ),
            )
        return cues
