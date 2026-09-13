"""A caption run has a maximum span (issue #970).

Similarity ends a run when the TEXT turns over. On uniform footage it never
does: the Apollo 11 moonwalk gave 2,190 captioned frames, 7 cues, and one cue
that covered 181 minutes, so the first step on the Moon and the President's
call both cited the same three hours. These tests pin the ceiling that bounds
it, and, just as carefully, pin that the overlay path it shares code with is
NOT bounded, because a banner on screen for twenty minutes really is one
sighting.
"""

import logging

import pytest

from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers.base import (
    RecognizerUnavailable,
    collapse_text_sightings,
)
from dirstral_annotator.recognizers.caption import (
    CAPTION_MAX_SPAN_S,
    SceneCaptionRecognizer,
)

MEDIA = "moonwalk.mp4"

#: The caption the Apollo run repeated for three hours, shortened. It is the
#: same words every frame, which is what makes similarity useless here.
GRAINY = "a grainy black-and-white image shows a tall, slender structure"


def uniform(text, start, stop, step, conf=0.9):
    """Sightings of ONE unchanging text, one every `step` seconds."""
    return [(float(t), text, conf) for t in range(start, stop, step)]


# --- the shared collapser: bounded only when asked -------------------------


def test_without_a_ceiling_three_hours_of_one_text_is_one_cue():
    """The bug, pinned. This is also the overlay contract, so it must stay."""
    cues = collapse_text_sightings(
        uniform(GRAINY, 0, 10800, 5), source="caption", event="scene_other",
        frame_gap=5.0,
    )
    assert len(cues) == 1
    assert cues[0].end_s - cues[0].start_s == pytest.approx(10800.0)


def test_a_ceiling_cuts_the_same_reads_into_citable_cues():
    cues = collapse_text_sightings(
        uniform(GRAINY, 0, 10800, 5), source="caption", event="scene_other",
        frame_gap=5.0, max_span=120.0,
    )
    assert len(cues) == 90
    assert max(c.end_s - c.start_s for c in cues) == pytest.approx(120.0)
    # Every second of the footage is still spoken for: the cuts abut, they do
    # not drop frames between pieces.
    assert cues[0].start_s == 0.0
    for earlier, later in zip(cues, cues[1:]):
        assert later.start_s == pytest.approx(earlier.end_s)


def test_the_ceiling_counts_the_trailing_extension_too():
    """A cue may not come back longer than the stated ceiling, gap included."""
    for max_span in (10.0, 12.5, 30.0, 120.0):
        cues = collapse_text_sightings(
            uniform(GRAINY, 0, 600, 5), source="caption", event="scene_other",
            frame_gap=5.0, max_span=max_span,
        )
        assert cues
        assert max(c.end_s - c.start_s for c in cues) <= max_span


def test_a_cut_piece_carries_the_text_its_own_frames_produced():
    """Each piece re-anchors, so it is not one caption stamped on every cut."""
    sightings = (
        [(float(t), "a control room with operators at consoles", 0.9)
         for t in range(0, 200, 5)]
        + [(float(t), "a control room with operators at consoles and a screen", 0.9)
           for t in range(200, 400, 5)]
    )
    cues = collapse_text_sightings(
        sightings, source="caption", event="scene_other",
        frame_gap=5.0, max_span=100.0,
    )
    assert len(cues) == 4
    # The two halves read differently, and the cues say so rather than
    # repeating the first read across all four.
    texts = [c.text for c in cues]
    assert texts[0] == "a control room with operators at consoles"
    assert texts[-1] == "a control room with operators at consoles and a screen"


def test_a_ceiling_below_one_cue_is_refused_at_the_call():
    with pytest.raises(ValueError, match="below"):
        collapse_text_sightings(
            uniform(GRAINY, 0, 60, 5), source="caption", event="scene_other",
            frame_gap=5.0, max_span=2.0,
        )


def test_a_ceiling_never_merges_what_the_text_already_separated():
    """The cap only ever cuts. Two different passages stay two cues."""
    sightings = ([(0.0, "the flag is planted beside the lander", 0.9)]
                 + [(5.0, "a view of Earth above a barren surface", 0.9)])
    for max_span in (None, 15.0):
        cues = collapse_text_sightings(
            sightings, source="caption", event="scene_other",
            frame_gap=5.0, max_span=max_span,
        )
        assert len(cues) == 2


# --- the recognizer: a ceiling by default ----------------------------------


@pytest.fixture
def fake_frames(monkeypatch, tmp_path):
    """Drive the recognizer off a scripted list of per-frame captions."""

    def install(captions, fps=0.2):
        frames = []
        for i in range(len(captions)):
            path = tmp_path / f"frame-{i}.jpg"
            path.write_text(str(i))
            frames.append((i / fps, path))
        monkeypatch.setattr(caption_mod, "iter_frames", lambda *a, **k: iter(frames))

        def captioner(paths):
            return [captions[int(p.read_text())] for p in paths]

        return captioner

    return install


def test_the_recognizer_bounds_a_static_recording_by_default(fake_frames):
    captioner = fake_frames([(GRAINY, 0.9)] * 240)  # 240 frames at 0.2 fps = 20 min
    cues = SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert len(cues) > 1
    assert max(c.end_s - c.start_s for c in cues) <= CAPTION_MAX_SPAN_S


def test_the_old_unbounded_behaviour_is_still_reachable(fake_frames):
    captioner = fake_frames([(GRAINY, 0.9)] * 240)
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=0.2, max_span=None,
    ).recognize(MEDIA)
    assert len(cues) == 1


#: Eight captions of genuinely different shots, in the wording the Apollo run
#: produced. They share only the boilerplate opening, which is exactly the case
#: CAPTION_RUN_SIMILARITY was raised to 0.75 to separate.
SHOTS = (
    "a dimly lit control room with multiple operators at consoles",
    "a lunar module footpad resting on fine grey soil",
    "an astronaut in a white suit descends a ladder",
    "the United States flag stands beside a deep footprint",
    "a view of Earth as a bright crescent above the horizon",
    "a television monitor showing wavy interference bands",
    "two figures set up a reflector experiment on flat ground",
    "a black screen with no picture at all",
)


def test_a_ceiling_does_not_split_footage_the_captioner_called_changing(fake_frames):
    """Guard against over-cutting: distinct shots are unaffected by the cap.

    Each shot here runs 15 s, far under the ceiling, so the cap must not be
    what decides the cue count. If it ever splits these, the ceiling has become
    a sampler rather than a bound.
    """
    captions = [(shot, 0.9) for shot in SHOTS for _ in range(3)]
    captioner = fake_frames(captions, fps=0.2)
    bounded = SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    unbounded = SceneCaptionRecognizer(
        captioner=captioner, fps=0.2, max_span=None).recognize(MEDIA)
    assert len(bounded) == len(SHOTS)
    assert [(c.start_s, c.end_s, c.text) for c in bounded] == \
        [(c.start_s, c.end_s, c.text) for c in unbounded]


@pytest.mark.parametrize("bad", [0, -1.0, 0.0, float("nan"), float("inf")])
def test_a_ceiling_that_no_cue_could_meet_is_refused_at_construction(fake_frames, bad):
    captioner = fake_frames([(GRAINY, 0.9)] * 4)
    with pytest.raises(RecognizerUnavailable, match="max_span"):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2, max_span=bad)


def test_the_run_says_what_a_cue_swallowed(fake_frames, caplog):
    captioner = fake_frames([(GRAINY, 0.9)] * 240)
    with caplog.at_level(logging.INFO, logger=caption_mod.__name__):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    report = "\n".join(r.getMessage() for r in caplog.records)
    assert "240 frames collapsed to" in report
    assert "the widest covers" in report
    # And it names the ceiling as the reason, so an operator does not have to
    # read the spans by eye to find out (which is how #970 was found).
    assert "reached the 120s ceiling" in report


def test_no_ceiling_warning_when_nothing_reached_it(fake_frames, caplog):
    captions = [(shot, 0.9) for shot in SHOTS for _ in range(2)]
    captioner = fake_frames(captions, fps=0.2)
    with caplog.at_level(logging.INFO, logger=caption_mod.__name__):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert "ceiling" not in "\n".join(r.getMessage() for r in caplog.records)


# --- the operator's two ways to set it -------------------------------------


def _serve_args(*extra):
    from dirstral_annotator.cli import build_parser

    return build_parser().parse_args(
        ["serve", "--roster", "r.json", "--caption", *extra])


def test_a_ceiling_on_the_command_line_reaches_the_recognizer():
    from dirstral_annotator.cli import _pipeline
    from dirstral_annotator.roster import Roster

    pipeline = _pipeline(_serve_args("--caption-max-span", "45"), Roster([]), {})
    assert pipeline.caption_max_span == 45.0
    built = SceneCaptionRecognizer(
        captioner=lambda _paths: [], fps=1.0, max_span=pipeline.caption_max_span)
    assert built.max_span == 45.0


def test_zero_on_the_command_line_removes_the_ceiling():
    """`--caption-max-span 0` is a request for no ceiling, not an absent flag.

    Zero is falsy, so the is-not-None threading is the whole test: read as
    unset it would leave the default in place and the operator who asked for
    the old unbounded behaviour would silently not get it.
    """
    from dirstral_annotator.cli import _pipeline
    from dirstral_annotator.roster import Roster

    pipeline = _pipeline(_serve_args("--caption-max-span", "0"), Roster([]), {})
    assert pipeline.caption_max_span == 0.0

    def captioner(paths):
        return [(GRAINY, 0.9)] * len(paths)

    pipeline.caption_fn = captioner
    built = pipeline._shared(
        "caption", ("k",),
        lambda: SceneCaptionRecognizer(
            captioner=captioner, fps=0.2,
            **({} if pipeline.caption_max_span is None
               else {"max_span": pipeline.caption_max_span or None}),
        ),
    )
    assert built.recognizer.max_span is None


def test_the_pipeline_rebuilds_the_recognizer_when_the_ceiling_changes():
    """The ceiling joins the cache key, so a served pipeline that is
    reconfigured stops emitting three-hour cues at the next request instead of
    for the life of the process."""
    from dirstral_annotator.pipeline import Pipeline
    from dirstral_annotator.roster import Roster

    def captioner(paths):
        return [(GRAINY, 0.9)] * len(paths)

    pipe = Pipeline(roster=Roster([]), games={}, caption_fn=captioner)
    keys = set()
    for span in (30.0, 60.0, 30.0):
        pipe.caption_max_span = span
        keys.add((pipe.caption_fn, pipe.probe_fn, pipe.caption_fps,
                  pipe.caption_windows, pipe.caption_floor_fps,
                  pipe.caption_prefix, pipe.caption_max_span))
    assert len(keys) == 2

    first = pipe._shared("caption", ("span", 30.0),
                         lambda: SceneCaptionRecognizer(
                             captioner=captioner, fps=0.2, max_span=30.0))
    second = pipe._shared("caption", ("span", 60.0),
                          lambda: SceneCaptionRecognizer(
                              captioner=captioner, fps=0.2, max_span=60.0))
    assert first.recognizer.max_span == 30.0
    assert second.recognizer.max_span == 60.0
    assert first is not second


# --- three things the ceiling changes elsewhere ----------------------------


def test_the_report_counts_each_cue_s_own_frames(fake_frames, caplog):
    """A frame at a seam belongs to ONE cue.

    240 frames at 0.2 fps under the 120 s ceiling is 24 reads per cue
    (t = 0 s to 115 s, then the next run opens at 120 s). Counting the span
    inclusively at both ends would claim 25, because the seam frame at 120 s
    is the next cue's first read.
    """
    captioner = fake_frames([(GRAINY, 0.9)] * 240)
    with caplog.at_level(logging.INFO, logger=caption_mod.__name__):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert "from 24 frames" in "\n".join(r.getMessage() for r in caplog.records)


def test_the_claim_gate_never_reads_a_neighbour_s_frame(fake_frames):
    """The gate publishes a claim when ANY frame in the run supports it, so a
    frame from the next cue is a frame that could carry a claim about footage
    this cue never showed. Cues abut at every ceiling cut, so the seam is the
    ordinary case now rather than a rare one."""
    crowd = "a crowd of spectators fills the stands behind the plate"
    captioner = fake_frames([(crowd, 0.9)] * 60, fps=0.2)
    probed = []

    def prober(paths, _question):
        probed.extend(paths)
        return [0.0] * len(paths)

    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=0.2, max_span=60.0, prober=prober,
    ).recognize(MEDIA)

    assert len(cues) > 1
    # 60 s ceiling at 0.2 fps is 12 reads per cue, and 60 frames is 5 cues.
    # Every frame is probed exactly once; a seam read twice means a cue was
    # judged partly on its neighbour.
    assert len(probed) == len(set(probed)) == 60


@pytest.mark.parametrize("bad", [True, False])
def test_a_boolean_is_not_a_span(fake_frames, bad):
    """`max_span=True` is 1.0 s, a ceiling no cue could meet, and it would fail
    deep inside a run that has already cost GPU hours."""
    captioner = fake_frames([(GRAINY, 0.9)] * 4)
    with pytest.raises(RecognizerUnavailable, match="max_span"):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2, max_span=bad)


@pytest.mark.parametrize(
    "bad, why",
    [(float("nan"), "nan"), (float("inf"), "inf"), (float("-inf"), "-inf")],
)
def test_the_collapser_refuses_a_span_that_is_not_a_number(bad, why):
    """The recognizer screens these, but the helper is public and the ticker
    path calls it directly.

    NaN is the dangerous one, and it fails quietly rather than loudly: every
    comparison against it is false, so the join test is false for EVERY read
    and each read opens its own run. Measured before the guard: 20 reads of one
    unchanging passage came back as 20 cues, the exact opposite of the ceiling
    that was asked for. Infinity is a second spelling of unbounded, and None is
    the one this contract has.
    """
    with pytest.raises(ValueError, match="finite"):
        collapse_text_sightings(
            uniform(GRAINY, 0, 100, 5), source="caption", event="scene_other",
            frame_gap=5.0, max_span=bad,
        )
