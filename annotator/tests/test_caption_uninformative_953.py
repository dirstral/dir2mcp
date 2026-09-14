"""A caption that describes nothing produces no cue (issue #953).

Measured on the pilot corpus right after the caption-gate deploy: the
re-annotation added 1,373 scene cues, and a share of them said things like "a
completely black screen with no visible content" or "a heavily pixelated and
distorted view of a stadium". Those are indexed on their TEXT, so they compete
with real moments in retrieval and pad the corpus.

The issue's acceptance criterion is that no labelled positive (celebration,
crowd, play) is dropped. These tests pin that it holds by CONSTRUCTION rather
than by measurement: the rule is consulted only for a caption that
classify_scene already mapped to scene_other, so a caption naming a real scene
survives whatever else it contains.
"""

import logging

import pytest

from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers.caption import (
    SCENE_CELEBRATION,
    SCENE_CROWD,
    SCENE_OTHER,
    SceneCaptionRecognizer,
    classify_scene,
    scene_is_uninformative,
)

MEDIA = "game.mp4"


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


# --- the classifier itself --------------------------------------------------


@pytest.mark.parametrize("caption", [
    "The broadcast frame shows a completely black screen with no visible content",
    "the frame is a blank screen",
    "a heavily pixelated and distorted view of a stadium",
    "the overlay text is unreadable",
    "the frame shows colour bars",
    "a test pattern fills the frame",
])
def test_a_frame_with_no_content_is_uninformative(caption):
    assert scene_is_uninformative(caption)


@pytest.mark.parametrize("caption", [
    # The Apollo 11 corpus, and the reason every phrase in the list is long.
    # A bare "black" needle would have dropped this footage whole: every
    # caption of it opens the same way (#970).
    "a grainy black-and-white image shows a tall, slender structure",
    "a restored television broadcast frame from July 20, 1969",
    "a black-and-white view of the lunar surface",
    "the camera is on the field behind home plate",
    "a wide aerial view of Oracle Park during the afternoon",
])
def test_a_frame_with_content_is_not_uninformative(caption):
    assert not scene_is_uninformative(caption)


# --- the ordering that makes the rule safe ----------------------------------


@pytest.mark.parametrize("caption, event", [
    ("the crowd is barely visible on a heavily pixelated frame", SCENE_CROWD),
    ("players celebrating, the frame is blurry and unreadable", SCENE_CELEBRATION),
])
def test_a_labelled_positive_survives_even_when_it_reads_as_degraded(fake_frames, caption, event):
    """The issue forbids dropping a labelled positive, and this is how that is
    guaranteed: the phrases are only consulted once classify_scene has ALREADY
    returned scene_other. Checking them first would discard these two."""
    assert scene_is_uninformative(caption), "the phrase really is present"
    assert classify_scene(caption) == event, "and the caption really is a positive"

    captioner = fake_frames([(caption, 0.9)] * 3)
    cues = SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert len(cues) == 1, "a labelled positive must never be dropped"
    assert cues[0].event == event


def test_an_uninformative_scene_other_caption_produces_no_cue(fake_frames):
    captioner = fake_frames([("a completely black screen with no visible content", 0.9)] * 3)
    cues = SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert cues == []


def test_only_the_empty_frames_are_dropped(fake_frames):
    """A run of real scenes with black frames between them keeps the scenes."""
    captions = [
        ("an astronaut in a white suit descends a ladder", 0.9),
        ("a completely black screen with no visible content", 0.9),
        ("the United States flag stands beside a deep footprint", 0.9),
        ("the frame is a blank screen", 0.9),
    ]
    captioner = fake_frames(captions, fps=0.2)
    cues = SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    texts = " | ".join(c.text for c in cues)
    assert len(cues) == 2, texts
    assert "ladder" in texts and "flag" in texts
    assert "black screen" not in texts and "blank screen" not in texts


def test_the_operator_can_keep_them(fake_frames):
    """A rule that removes cues must be switchable, and the run says how."""
    captions = [("a completely black screen with no visible content", 0.9)] * 3
    captioner = fake_frames(captions, fps=0.2)
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=0.2, drop_uninformative=False).recognize(MEDIA)
    assert len(cues) == 1
    assert cues[0].event == SCENE_OTHER


def test_the_run_says_how_many_it_dropped(fake_frames, caplog):
    """A rule that silently removes cues is one an operator cannot audit, and
    the count is the only way to notice it removing too much."""
    captions = [("a completely black screen with no visible content", 0.9)] * 3
    captioner = fake_frames(captions, fps=0.2)
    with caplog.at_level(logging.INFO, logger=caption_mod.__name__):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    report = "\n".join(r.getMessage() for r in caplog.records)
    assert "1 cue(s) described nothing citable" in report
    assert "--caption-keep-uninformative" in report


def test_a_clean_run_says_nothing_about_dropping(fake_frames, caplog):
    captioner = fake_frames([("an astronaut descends a ladder", 0.9)] * 3, fps=0.2)
    with caplog.at_level(logging.INFO, logger=caption_mod.__name__):
        SceneCaptionRecognizer(captioner=captioner, fps=0.2).recognize(MEDIA)
    assert "described nothing citable" not in "\n".join(r.getMessage() for r in caplog.records)


# --- and the flag reaches it from argv ---------------------------------------


def test_the_opt_out_reaches_the_pipeline_from_argv():
    from dirstral_annotator.cli import _pipeline, build_parser
    from dirstral_annotator.roster import Roster

    def args(*extra):
        return build_parser().parse_args(["serve", "--roster", "r.json", *extra])

    # Absent: the recognizer's own default stands, rather than being asserted.
    assert _pipeline(args(), Roster([]), {}).caption_drop_uninformative is None
    assert _pipeline(args("--caption-keep-uninformative"), Roster([]), {}).caption_drop_uninformative is False
