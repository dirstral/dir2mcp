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

The play-by-play recognizer is the one that is still built per request, because
it is keyed per media file and holds no model. What made that expensive was its
LABELS, not its construction: it read the game feed every time. That read is now
a memo on the binding it came from. See `GameConfig.events` (#844).
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

    def __post_init__(self) -> None:
        # Not dataclass fields on purpose, as in `Pipeline`: a Lock is neither
        # comparable nor copyable, and a game's parsed events have no place in a
        # repr of the configuration an operator wrote.
        self._read_lock = threading.Lock()
        self._parsed: tuple[tuple, list[ground_truth.PitchEvent]] | None = None

    @classmethod
    def parse(cls, raw: dict) -> "GameConfig":
        anchors = []
        for spec in raw.get("anchors", []):
            epoch, video = str(spec).split("=", 1)
            anchors.append(align.Anchor(epoch_s=float(epoch), video_s=float(video)))
        return cls(anchors=anchors, game_pk=raw.get("game_pk"), feed=raw.get("feed"))

    def events(self) -> list[ground_truth.PitchEvent]:
        """The feed's pitches, read at most once per binding (#844).

        `Pipeline.cues_for` builds `PlayByPlayRecognizer` per request, so this
        ran per request. Measured on a real 0.901 MB GUMBO feed of 344 pitches:
        15.33 ms for the saved file, 2892 ms (up to 5953 ms) for the `game_pk`
        fetch. The file read is noise beside the 46 s that #650 removed; the
        fetch is seconds of latency per request, spent on a service that has no
        reason to be live for a recognizer to run.

        This is a memo on the binding, not a cache with an eviction policy.
        What stays resident is bounded by the bindings games.json declares,
        which already live as long as the pipeline: 69.5 KiB of parsed events for
        that 344 pitch game, and the 2.77 MiB raw payload is parsed and dropped.

        Keyed on the binding, so repointing `feed` or `game_pk` is picked up, the
        same rule the recognizer cache follows. A binding that still points at the
        same path keeps the parse it made: every request in a run then answers
        from one label set, instead of from whichever request happened to re-read
        a file that changed. An operator who wants a changed file reloads the
        bindings.

        Only a success is remembered. A read that fails raises as it did before,
        and the next request tries again, because a remembered failure would turn
        one bad moment into a process that never recognizes play-by-play again.
        The lock means two concurrent first requests do not both fetch: the second
        waits and then reads the memo.
        """
        with self._read_lock:
            # The binding is read under the lock, and the read below uses those
            # locals. A binding sampled before the wait could be repointed while
            # this request queues, which would file the NEW feed's events under
            # the OLD key.
            feed, game_pk = self.feed, self.game_pk
            binding = (feed, game_pk)
            memo = self._parsed
            if memo is None or memo[0] != binding:
                raw = (
                    ground_truth.load_game(feed) if feed
                    else ground_truth.fetch_game(game_pk)
                )
                memo = (binding, ground_truth.parse_pitches(raw))
                self._parsed = memo
            # A copy of a few hundred pointers, because the memo is now shared by
            # every request: one caller sorting or filtering in place would
            # otherwise change what every later request sees. `PitchEvent` is
            # frozen, so the events themselves need no copy.
            return list(memo[1])

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
