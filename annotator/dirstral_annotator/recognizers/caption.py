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

THE CLAIM GATE (#923), and why it is not a confidence threshold. #860 left the
gate unshipped because no labelled set existed. One now does: 400 uniformly
sampled frames from the pilot game, hand labelled. It says two things.

First, the caption is mostly RIGHT. Scene type measured ~0.94. What fails is
one clause inside an otherwise correct sentence, so any WHOLE-CAPTION score is
dominated by the correct majority and cannot isolate the wrong clause. Measured
over those 400 frames, as area under the ROC curve for separating true from
false reaction claims:

    self-reported confidence   0.514   <- what a `min_confidence` gate reads
    caption min-token logprob  0.507
    caption mean-token logprob 0.338   <- below chance: fluent captions are, if
    claim-span logprob         0.247      anything, slightly MORE likely wrong
    targeted binary probe      0.991

The self-reported number returned exactly 0.95 on 96.7% of frames, including a
caption that invented a team and a score. Thresholding it can only pass
everything or drop everything. That is why `min_confidence` survives as a
backend-liveness knob and is NOT the gate.

Second, asking the claim DIRECTLY works. A yes/no question about the exact
claim, scored by the probability of the answer token, is a scalar attached to
the thing that fails. Gating on it moved the reaction claim from 0.35 precision
to 0.92 on that sample.

So the gate is a ProbeFn, injected like the captioner, and it guards the two
events that carry a claim rather than a description. Scene type keeps coming
from the caption, because at ~0.94 it needs no gate.

WITHOUT A PROBER the recognizer behaves exactly as it did before: the events
pass ungated. That is capability-driven activation, not a silent default, and
the cost of leaving it off is the 0.35 precision measured above.
"""

from __future__ import annotations

import logging
import math
import re
from collections.abc import Callable, Iterable
from pathlib import Path

from ..model import Cue
from .base import RecognizerUnavailable, collapse_text_sightings, iter_frames, scrub_error

log = logging.getLogger(__name__)

#: A batch of frame images in, one (text, confidence) pair per frame out. The
#: batch shape is the interface because #860 measured batching as the lever
#: that matters: a batch of 8 cut the per-frame cost 2.3x on the pilot's A2,
#: while halving the pixel budget changed the ANSWER rather than the price.
CaptionFn = Callable[[list[Path]], list[tuple[str, float]]]

#: A batch of frames and ONE yes/no question in, one P(yes) per frame out.
#: The question is asked about the specific claim being gated, and the float is
#: the probability the backend assigns to answering "yes". Batch-shaped for the
#: same reason CaptionFn is. Supplying this activates the #923 claim gate.
ProbeFn = Callable[[list[Path], str], list[float]]

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
#: The gated events and the question that decides each. Only events that assert
#: something ABOUT the people in frame are here. scene_field, scene_graphic,
#: scene_replay and scene_dugout describe where the camera is pointing, which
#: measured ~0.94 and needs no gate.
#:
#: The wording is the measured wording. These are the exact questions scored in
#: the 400-frame run, so a reworded question invalidates the threshold below.
CLAIM_PROBES: dict[str, str] = {
    SCENE_CELEBRATION: (
        "Look at this broadcast frame. Are players visibly celebrating "
        "(high-fives, embraces, raised arms)? Answer with one word: yes or no."
    ),
    SCENE_CROWD: (
        "Look at this broadcast frame. Is the camera showing the crowd or "
        "stands as the main subject? Answer with one word: yes or no."
    ),
}

#: Default claim threshold. Deliberately severe, and the reason is the corpus
#: this feeds: a WRONG span is worse than a MISSING one, because a false
#: "crowd celebrating" is served against a timestamp as though the archive
#: recorded it, while a missed one only leaves the moment unlabelled.
#:
#: Measured on the 400-frame set, per gated event, at this threshold:
#:     scene_celebration   precision 0.83, recall 0.63
#:     scene_crowd         precision 0.86, recall 0.43
#:
#: Honest about the sample: it holds 14 positives, so the difference between
#: 0.90 and 0.99 is a single frame and is NOT statistically separable. 0.99 is
#: chosen on the precision-first principle above, not because the data
#: distinguishes it. An operator who would rather find more and verify by hand
#: should lower it; the recall column is what they buy.
CLAIM_THRESHOLD = 0.99

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


def _is_probability(value: object) -> bool:
    """True when `value` is a real number in [0, 1].

    bool is rejected on purpose: `True >= 0.99` is a silent pass, and a backend
    that returns a bare yes/no has not supplied the confidence this gate reads.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(value) and 0.0 <= float(value) <= 1.0


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
        floor_fps: float | None = None,
        similarity: float = CAPTION_RUN_SIMILARITY,
        # Keyword-only, and appended rather than inserted: the positional
        # contract predates #923, and a caller passing `windows` positionally
        # would otherwise bind its tuple to `prober` and fail deep inside a
        # gated run instead of at the call site.
        *,
        prober: ProbeFn | None = None,
        claim_threshold: float = CLAIM_THRESHOLD,
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
        # #923. None leaves the claim events ungated, which is what shipped
        # before and measured 0.35 precision on the reaction claim. Present
        # activates the gate; nothing else switches it on, so the capability
        # follows the backend rather than a flag that can disagree with it.
        self.prober = prober
        # A threshold outside [0,1] silently inverts the gate: below zero every
        # score clears it, so the check would publish exactly the claims it
        # exists to withhold. Rejected at construction, where the operator can
        # still see it, rather than at the first crowd shot.
        if not _is_probability(claim_threshold):
            raise RecognizerUnavailable(
                "caption claim_threshold must be a real number in [0, 1], got "
                f"{claim_threshold!r}"
            )
        self.claim_threshold = float(claim_threshold)
        # Aimed windows (#860 tier A). Uniform sampling MEASURED 0 of 8 frames
        # showing a celebration or crowd reaction, against 6 of 8 for frames
        # aimed at high-captivatingIndex plays, so aiming is the difference
        # between a capability and a demo that finds nothing. None means
        # caption every sampled frame (tier B, the floor).
        self.windows = tuple(windows) if windows is not None else None
        # Tier B, the floor (#860). Aiming alone is an EXCLUSIVE filter, and
        # #860 measured why that loses the capability: 54 of the 84 plays are
        # invisible to the aimed tier, and the one fan cutaway found in the
        # game (a bodyboard rider at t=2294 s) sits in dead time BETWEEN plays,
        # which is exactly where a broadcast puts its human-interest shots. So
        # a low rate across the whole file runs alongside the windows, never
        # instead of them. None means no floor, which is only correct when the
        # caller has no windows either (then every frame is already captioned).
        self.floor_fps = floor_fps
        self.similarity = similarity

    def _in_window(self, t: float) -> bool:
        if self.windows is None:
            return True
        return any(start <= t <= end for start, end in self.windows)

    def _select(self, frames: list[tuple[float, Path]]) -> list[tuple[float, Path]]:
        """Pick the frames to caption: every aimed frame, plus a floor tick.

        One extraction pass feeds both tiers, so the floor costs decode it was
        already paying and adds only its own model time. Selection is by
        timestamp, so a frame that satisfies both tiers is captioned once: the
        deduplication is structural rather than a later pass.
        """
        if self.windows is None:
            return frames
        step = 1.0 / self.floor_fps if self.floor_fps else None
        next_tick = 0.0
        selected: list[tuple[float, Path]] = []
        for t, path in frames:
            aimed = self._in_window(t)
            floored = step is not None and t >= next_tick
            if floored:
                # Advance past t so a dense aimed window cannot consume many
                # ticks at once and starve the rest of the file.
                next_tick = t + step
            if aimed or floored:
                selected.append((t, path))
        return selected

    def _claim_holds(self, event: str, frames: list[Path]) -> bool:
        """Ask the backend directly whether the claim in `event` is true.

        A run is several frames, and the claim is about the run, so the run
        passes when its STRONGEST frame passes. Max rather than mean is
        deliberate: a celebration that occupies two frames of a six-frame run
        is still a celebration, and averaging would let the quiet frames
        outvote the one that carries the event.
        """
        question = CLAIM_PROBES[event]
        scores = self.prober(frames, question)
        if len(scores) != len(frames):
            raise RecognizerUnavailable(
                f"probe backend returned {len(scores)} scores for "
                f"{len(frames)} frames; the contract is one P(yes) per frame"
            )
        for score in scores:
            # A backend that answers inf, NaN or 1.7 is not answering the
            # question asked. Trusting it would clear any threshold and publish
            # an unverified claim, so a malformed probe is unavailability, not
            # a yes.
            if not _is_probability(score):
                raise RecognizerUnavailable(
                    "probe backend returned a score outside the probability "
                    f"domain: {score!r}"
                )
        return any(float(score) >= self.claim_threshold for score in scores)

    def recognize(self, media_path: Path) -> list[Cue]:
        frames = self._select(list(iter_frames(media_path, fps=self.fps)))
        sightings: list[tuple[float, str, float]] = []
        # Timestamp -> frame, so a collapsed run can be re-probed on the exact
        # frames it came from rather than on a re-extraction.
        frame_at: dict[float, Path] = {}
        n_batches = failed_batches = 0
        last_error: BaseException | None = None
        for i in range(0, len(frames), self.batch_size):
            batch = frames[i:i + self.batch_size]
            n_batches += 1
            try:
                results = self.captioner([path for _, path in batch])
            except RecognizerUnavailable:
                raise
            except Exception as exc:  # noqa: BLE001 - the whole point is ANY fault
                # A backend fault on ONE batch (a CUDA error, a decode error on
                # one frame, a transient in a remote captioner) used to abort
                # the run and discard every caption before it; on the pilot
                # that was four GPU-hours (#945). Skip the batch, keep going,
                # and say so. A backend that fails EVERY batch is dead, and that
                # is unavailability (below), never a silent zero captions.
                failed_batches += 1
                last_error = exc
                log.warning(
                    "caption batch %d (%d frames from t=%.1fs) failed and was skipped: %s",
                    n_batches, len(batch), batch[0][0], scrub_error(exc),
                )
                continue
            if len(results) != len(batch):
                raise RecognizerUnavailable(
                    "caption backend returned "
                    f"{len(results)} results for {len(batch)} frames; the "
                    "contract is one (text, confidence) pair per frame"
                )
            for (t, fpath), (text, confidence) in zip(batch, results):
                text = " ".join(str(text).split())
                if not text or confidence < self.min_confidence:
                    continue
                frame_at[t] = fpath
                # The RAW caption is what gets compared: prefixing first would
                # add a constant this module chose to every side of every
                # comparison, and measurably did (0.500 -> 0.706), making the
                # module's own words the reason two different shots merged.
                sightings.append((t, text, float(confidence)))

        if n_batches and failed_batches == n_batches:
            raise RecognizerUnavailable(
                f"caption backend failed on all {n_batches} batches; last error: "
                f"{scrub_error(last_error)}"
            )
        if failed_batches:
            log.warning(
                "caption: %d of %d batches failed and were skipped; the run is missing "
                "those frames", failed_batches, n_batches,
            )

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
            # The event is classified from the surviving text, so a collapsed
            # run is labelled by the passage it kept rather than by whichever
            # frame happened to be first.
            event = classify_scene(cue.text)
            # #923. An event that ASSERTS something about the people in frame
            # is checked against the frames before it is published. The caption
            # keeps its prose either way: what the gate withdraws is the
            # FILTERABLE claim, which is what "show me the crowd reaction"
            # searches and therefore what a false positive corrupts. Demoted to
            # scene_other rather than dropped, because the passage still
            # describes a real moment and the timestamp is still worth keeping.
            if self.prober is not None and event in CLAIM_PROBES:
                run = [
                    frame_at[t]
                    for t, _, _ in sightings
                    if cue.start_s <= t <= cue.end_s and t in frame_at
                ]
                if run and not self._claim_holds(event, run):
                    event = SCENE_OTHER
            cues.append(
                Cue(
                    source=cue.source,
                    start_s=cue.start_s,
                    end_s=cue.end_s,
                    event=event,
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
