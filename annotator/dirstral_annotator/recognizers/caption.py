"""Scene caption recognizer: describe what a frame SHOWS (issue #860, part of #741).

Every other recognizer resolves an IDENTITY or reads a TEXT OVERLAY:
play-by-play knows who pitched, the scorebug reads who is on screen now, the
jersey reader reads a number, the face matcher matches a rostered person. A
crowd shot has no feed row, no overlay, no jersey and no rostered face, so
"show me the crowd reaction" and "best fan moments" are unanswerable today.
That is structural, not a tuning problem, which is why this is a recognizer
rather than a threshold change.

WHAT THIS MODULE SHIPS. The recognizer, its closed event vocabulary, its run
collapsing and its unavailability contract. The heavy backend is an injected
callable, exactly as `faces.py` injects `EmbedFn`, so the package still imports
with no vision-language model present and the whole thing is testable without a
GPU.

WHAT IT DELIBERATELY DOES NOT SHIP. A confidence gate. #860 measured the raw
model at 0.63 precision on the exact claim behind "show me the crowd reaction",
with all three failures being FALSE POSITIVES in the direction that sells the
feature (a seated, static crowd described as "cheering"). #751 found the same
shape on overlay text: 0.40 precision unfiltered, 0.90 after a MEASURED gate.
A gate invented here without the 300-frame labelled set #860 specifies would be
a number chosen to look good, so `min_confidence` stays an operator input with
no shipped default, and the caption text says plainly that it is generated.
"""

from __future__ import annotations

import re
from collections.abc import Callable, Iterable
from pathlib import Path

from ..model import Cue
from .base import RecognizerUnavailable, collapse_text_sightings, iter_frames

#: A batch of frame images in, one (text, confidence) pair per frame out. The
#: batch shape is the interface because #860 measured batching as the lever
#: that matters: a batch of 8 cut the per-frame cost 2.3x on the pilot's A2,
#: while halving the pixel budget changed the ANSWER rather than the price.
CaptionFn = Callable[[list[Path]], list[tuple[str, float]]]

#: The closed event vocabulary. `event` is what retrieval FILTERS on (design
#: 0004 section 6.2, and recognitionSegments persists it on the chunk span), so
#: a free-form per-frame event string would make the filter useless. The prose
#: stays in `text`; only this bounded set reaches `event`.
#:
#: Deliberately disjoint from the feed's `pitch` and `at_bat`, so a client can
#: ask for feed events only, or for generated scene events only, and never has
#: to guess which kind it got.
SCENE_FIELD = "scene_field"
SCENE_CROWD = "scene_crowd"
SCENE_CELEBRATION = "scene_celebration"
SCENE_DUGOUT = "scene_dugout"
SCENE_GRAPHIC = "scene_graphic"
SCENE_REPLAY = "scene_replay"
SCENE_OTHER = "scene_other"

SCENE_EVENTS = (
    SCENE_FIELD,
    SCENE_CROWD,
    SCENE_CELEBRATION,
    SCENE_DUGOUT,
    SCENE_GRAPHIC,
    SCENE_REPLAY,
    SCENE_OTHER,
)

#: Prefix on every caption. A reader (and a model reading a retrieved chunk)
#: must be able to tell a GENERATED description from a recorded fact: the feed
#: says a home run happened, this says a frame looks like a celebration, and
#: #860 measured the latter wrong about a third of the time on that claim.
#: df-005's `derivation` field carries the same distinction structurally; this
#: carries it in the words, because the words are what reaches the answer.
CAPTION_PREFIX = "Scene (auto description, not the game feed): "

#: Similarity above which two consecutive captions are treated as one passage.
#:
#: HIGHER than base.TEXT_RUN_SIMILARITY (0.5), and the difference is measured
#: rather than chosen. That base value is calibrated for ticker passages, which
#: share no fixed opening, so any overlap is real content. Captions from one
#: model share a fixed opening: #860 recorded "This broadcast frame shows"
#: repeating across every frame. Measured on two captions of DIFFERENT shots:
#:
#:   this frame shows the pitcher ... / this frame shows fans in the stands ...
#:     boilerplate stripped : 0.167
#:     as the model emits it: 0.500   <- merges at the base threshold
#:
#: So the base threshold cannot separate two different shots once boilerplate
#: is present, and the failure is silent AND lossy: the run keeps one caption
#: and discards the other, so the crowd shot disappears from the corpus
#: entirely. 0.75 sits between that 0.5 and the ~1.0 of two captions of the
#: same shot.
#:
#: PROVISIONAL. Like every other number in #860 that has no labelled set behind
#: it yet, this one is a measured starting point, not a tuned result: the
#: 300-frame ground truth #860 specifies should confirm or replace it, because
#: too high a value splits one shot into several cues.
CAPTION_RUN_SIMILARITY = 0.75

#: Ordered classifier rules: the FIRST match wins, so the order is the
#: precedence. Celebration outranks crowd because a dugout celebration with
#: fans behind it is the celebration the user asked for; graphic and replay
#: outrank the rest because a caption that names an overlay is describing the
#: broadcast, not the ballpark.
_SCENE_RULES: tuple[tuple[str, tuple[str, ...]], ...] = (
    (SCENE_REPLAY, ("replay", "slow motion", "slow-motion")),
    (SCENE_GRAPHIC, ("graphic", "scoreboard graphic", "on-screen text",
                     "lower third", "broadcast overlay", "statistics overlay")),
    (SCENE_CELEBRATION, ("celebrat", "high five", "high-five", "fist bump",
                         "embrac", "hugging", "cheering teammates", "mobbed")),
    (SCENE_DUGOUT, ("dugout", "bench area")),
    (SCENE_CROWD, ("crowd", "fans", "spectators", "stands")),
    (SCENE_FIELD, ("pitcher", "batter", "infield", "outfield", "base ",
                   "mound", "home plate", "fielder")),
)


def classify_scene(caption: str) -> str:
    """Map a caption onto the closed vocabulary. Deterministic and a pure
    function of the text, so a fixture pins the mapping without a model."""
    text = " " + re.sub(r"\s+", " ", caption).strip().lower() + " "
    for event, needles in _SCENE_RULES:
        for needle in needles:
            if needle in text:
                return event
    return SCENE_OTHER


class SceneCaptionRecognizer:
    """Describe sampled frames and emit one cue per described passage.

    `captioner` is required in practice and injected in tests. Passing None
    asks for the default vision-language adapter, which this module does not
    build: shipping a default that silently downloads multi-gigabyte weights
    would violate the same rule `face` follows, where the heavy dependency is
    an opt-in extra whose absence degrades through RecognizerUnavailable.
    """

    name = "caption"

    def __init__(
        self,
        captioner: CaptionFn | None = None,
        fps: float = 1.0,
        batch_size: int = 8,
        min_confidence: float = 0.0,
        windows: Iterable[tuple[float, float]] | None = None,
        similarity: float = CAPTION_RUN_SIMILARITY,
    ):
        if captioner is None:
            raise RecognizerUnavailable(
                "scene captioning needs a vision-language backend: install the "
                "`caption` extra and pass a CaptionFn (no default adapter is "
                "bundled, because the weights are a multi-gigabyte opt-in)"
            )
        if fps <= 0:
            raise RecognizerUnavailable(f"caption fps must be positive, got {fps}")
        self.captioner = captioner
        self.fps = fps
        # #860 measured batching as the cost lever on the pilot GPU: a batch of
        # 8 cut the per-frame cost 2.3x because decode is bandwidth bound, so
        # the batch amortizes the weight reads.
        self.batch_size = max(1, int(batch_size))
        self.min_confidence = min_confidence
        # Aimed windows (#860 tier A). Uniform sampling MEASURED 0 of 8 frames
        # showing a celebration or crowd reaction, against 6 of 8 for frames
        # aimed at high-captivatingIndex plays, so aiming is the difference
        # between a capability and a demo that finds nothing. None means
        # caption every sampled frame (tier B, the floor).
        self.windows = tuple(windows) if windows is not None else None
        self.similarity = similarity

    def _in_window(self, t: float) -> bool:
        if self.windows is None:
            return True
        return any(start <= t <= end for start, end in self.windows)

    def recognize(self, media_path: Path) -> list[Cue]:
        frames: list[tuple[float, Path]] = [
            (t, frame) for t, frame in iter_frames(media_path, fps=self.fps)
            if self._in_window(t)
        ]
        sightings: list[tuple[float, str, float]] = []
        for i in range(0, len(frames), self.batch_size):
            batch = frames[i:i + self.batch_size]
            results = self.captioner([path for _, path in batch])
            if len(results) != len(batch):
                raise RecognizerUnavailable(
                    "caption backend returned "
                    f"{len(results)} results for {len(batch)} frames; the "
                    "contract is one (text, confidence) pair per frame"
                )
            for (t, _), (text, confidence) in zip(batch, results):
                text = " ".join(str(text).split())
                if not text or confidence < self.min_confidence:
                    continue
                # The RAW caption is what gets compared: prefixing first would
                # add a constant this module chose to every side of every
                # comparison, and measurably did (0.500 -> 0.706), making the
                # module's own words the reason two different shots merged.
                sightings.append((t, text, float(confidence)))

        # Collapse by TEXT, not by identity: a caption names nobody the roster
        # owns, so there is no entity to group on.
        #
        # This also disarms a fusion hazard #860 identified. fuse() groups two
        # entity-free cues when their text overlaps by TEXT_MERGE_SIMILARITY,
        # and captions share heavy boilerplate ("This broadcast frame shows"),
        # so adjacent captions of DIFFERENT shots would cross that threshold on
        # boilerplate alone and merge into one long annotation. Collapsing here
        # anchors each run on its FIRST read, which bounds the run, and it is
        # the same mechanism base.py already documents for ticker text.
        cues: list[Cue] = []
        for cue in collapse_text_sightings(
            sightings,
            source=self.name,
            event=SCENE_OTHER,
            frame_gap=1.0 / self.fps,
            similarity=self.similarity,
        ):
            cues.append(
                Cue(
                    source=cue.source,
                    start_s=cue.start_s,
                    end_s=cue.end_s,
                    # The event is classified from the surviving text, so a
                    # collapsed run is labelled by the passage it kept rather
                    # than by whichever frame happened to be first.
                    event=classify_scene(cue.text),
                    # Empty on purpose. A crowd has nobody the roster owns, and
                    # inventing `entity:crowd` would put a non-entity into the
                    # namespace the roster owns and into the section 6.2 entity
                    # filter, where a client would see it offered beside real
                    # players. base.py records the same decision for ticker text.
                    entity_ids=(),
                    confidence=cue.confidence,
                    # Prefixed only now, after collapsing has compared the raw
                    # captions, so the marker cannot influence grouping.
                    text=CAPTION_PREFIX + cue.text,
                )
            )
        return cues
