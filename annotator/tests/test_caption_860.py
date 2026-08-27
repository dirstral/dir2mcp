"""Scene caption recognizer (issue #860, part of #741).

Every other recognizer resolves an identity or reads an overlay, so a crowd
shot, which has no feed row, no overlay, no jersey and no rostered face, was
unanswerable. These tests pin the contract of the recognizer that describes
what a frame shows: its closed event vocabulary, its unavailability behaviour,
its run collapsing, and the two decisions that keep generated prose from being
mistaken for recorded fact.
"""

import pytest

from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.caption import (
    CAPTION_PREFIX,
    SCENE_CELEBRATION,
    SCENE_CROWD,
    SCENE_DUGOUT,
    SCENE_EVENTS,
    SCENE_FIELD,
    SCENE_GRAPHIC,
    SCENE_OTHER,
    SCENE_REPLAY,
    SceneCaptionRecognizer,
    classify_scene,
)

MEDIA = "game.mp4"


@pytest.fixture
def fake_frames(monkeypatch, tmp_path):
    """Drive the recognizer off a scripted list of per-frame captions."""

    def install(captions, fps=1.0):
        frames = []
        for i in range(len(captions)):
            path = tmp_path / f"frame-{i}.jpg"
            path.write_text(str(i))
            frames.append((i / fps, path))
        monkeypatch.setattr(caption_mod, "iter_frames", lambda *a, **k: iter(frames))

        seen = {"batches": []}

        def captioner(paths):
            seen["batches"].append(len(paths))
            out = []
            for p in paths:
                idx = int(p.read_text())
                text, conf = captions[idx]
                out.append((text, conf))
            return out

        return captioner, seen

    return install


def test_a_caption_becomes_a_classified_cue(fake_frames):
    captioner, _ = fake_frames([
        ("players celebrating with high fives in the dugout", 0.9),
    ])
    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0).recognize(MEDIA)
    assert len(cues) == 1
    cue = cues[0]
    assert cue.source == "caption"
    assert cue.event == SCENE_CELEBRATION
    assert cue.event in SCENE_EVENTS
    # A crowd has nobody the roster owns; inventing an id would put a
    # non-entity into the namespace the roster owns and into the entity filter.
    assert cue.entity_ids == ()


def test_the_caption_says_it_is_generated(fake_frames):
    """#860 measured 0.63 precision on 'the crowd is reacting', with every
    failure a false positive. A reader, and a model reading a retrieved chunk,
    must be able to tell this from a recorded feed fact."""
    captioner, _ = fake_frames([("the crowd in the stands is cheering", 0.9)])
    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0).recognize(MEDIA)
    assert cues[0].text.startswith(CAPTION_PREFIX)
    assert "not the game feed" in cues[0].text


@pytest.mark.parametrize(
    "text,want",
    [
        ("a slow motion replay of the swing", SCENE_REPLAY),
        ("a broadcast overlay showing the pitch count", SCENE_GRAPHIC),
        ("teammates celebrating at home plate", SCENE_CELEBRATION),
        ("players seated in the dugout", SCENE_DUGOUT),
        ("fans in the stands watching", SCENE_CROWD),
        ("the pitcher throws from the mound", SCENE_FIELD),
        ("a man on a yellow bodyboard in the water", SCENE_OTHER),
    ],
)
def test_classification_is_a_closed_vocabulary(text, want):
    assert classify_scene(text) == want
    assert classify_scene(text) in SCENE_EVENTS


def test_celebration_outranks_crowd(fake_frames):
    """A dugout celebration with fans behind it is the celebration the user
    asked for, so precedence is part of the contract, not an accident of
    dictionary order."""
    assert classify_scene(
        "a player is greeted with high fives in the dugout while fans watch from the stands"
    ) == SCENE_CELEBRATION


def test_adjacent_shots_do_not_merge_on_boilerplate(fake_frames):
    """The fusion hazard #860 identified: fuse() groups entity-free cues on
    text overlap, and captions share heavy boilerplate, so two DIFFERENT shots
    would merge into one long annotation on the shared words alone. Collapsing
    anchors each run on its first read, which bounds it."""
    captioner, _ = fake_frames([
        ("This broadcast frame shows the pitcher throwing from the mound", 0.9),
        ("This broadcast frame shows the pitcher throwing from the mound", 0.9),
        ("This broadcast frame shows fans in the stands waving towels", 0.9),
        ("This broadcast frame shows fans in the stands waving towels", 0.9),
    ])
    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0).recognize(MEDIA)
    assert len(cues) == 2, [c.text for c in cues]
    assert {c.event for c in cues} == {SCENE_FIELD, SCENE_CROWD}


def test_low_confidence_frames_are_dropped(fake_frames):
    """min_confidence is an operator input with NO shipped default: #860 has
    not yet produced the 300-frame labelled set a real gate needs, and a number
    invented here would be chosen to look good rather than measured."""
    captioner, _ = fake_frames([
        ("fans in the stands cheering", 0.2),
        ("the pitcher throws from the mound", 0.95),
    ])
    kept = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, min_confidence=0.5
    ).recognize(MEDIA)
    assert [c.event for c in kept] == [SCENE_FIELD]

    ungated = SceneCaptionRecognizer(captioner=captioner, fps=1.0).recognize(MEDIA)
    assert len(ungated) == 2, "no gate ships by default; both frames survive"


def test_windows_aim_the_captioner(fake_frames):
    """#860 measured uniform sampling finding 0 of 8 celebration or crowd
    frames, against 6 of 8 when aimed at high-captivatingIndex plays. Aiming is
    the difference between a capability and a demo that finds nothing, and it
    must also stop the model being paid for frames nobody asked about."""
    captioner, seen = fake_frames([
        ("the pitcher throws from the mound", 0.9),
        ("teammates celebrating at home plate", 0.9),
        ("an aerial view of the stadium", 0.9),
    ])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, windows=[(1.0, 1.0)]
    ).recognize(MEDIA)
    assert [c.event for c in cues] == [SCENE_CELEBRATION]
    # Exactly one frame was captioned, so the two outside the window cost nothing.
    assert sum(seen["batches"]) == 1


def test_batching_is_honoured(fake_frames):
    """Batch size is the measured cost lever (2.3x on the pilot GPU), so a
    recognizer that quietly captioned one frame at a time would be a
    performance regression invisible to every other assertion here."""
    captioner, seen = fake_frames([("the pitcher throws", 0.9)] * 10)
    SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=4).recognize(MEDIA)
    assert seen["batches"] == [4, 4, 2]


def test_no_backend_is_unavailable_not_a_crash():
    """The `caption` extra is opt-in and torch is heavy, so a deployment
    without it must degrade the way `face` does: the cascade records a skip and
    keeps going."""
    with pytest.raises(RecognizerUnavailable) as excinfo:
        SceneCaptionRecognizer(captioner=None)
    assert "caption" in str(excinfo.value).lower()


def test_a_backend_that_breaks_its_contract_is_reported(fake_frames):
    """One (text, confidence) pair per frame is the contract. A backend that
    returns a different count has silently misaligned captions with timestamps,
    which would attribute a description to the wrong moment."""
    captioner, _ = fake_frames([("a", 0.9), ("b", 0.9)])

    def short(paths):
        return captioner(paths)[:1]

    with pytest.raises(RecognizerUnavailable):
        SceneCaptionRecognizer(captioner=short, fps=1.0, batch_size=2).recognize(MEDIA)


def test_the_generated_marker_does_not_influence_grouping(fake_frames):
    """CAPTION_PREFIX is a constant this module adds to every caption, so
    comparing prefixed text would put the module's own words on both sides of
    every similarity test and inflate it. Measured: the same two
    different-shot captions score 0.500 raw and 0.706 prefixed.

    Pinned at a threshold BETWEEN those two numbers, so the test fails if the
    prefix is ever applied before collapsing, independently of where
    CAPTION_RUN_SIMILARITY happens to sit.
    """
    captioner, _ = fake_frames([
        ("This broadcast frame shows the pitcher throwing from the mound", 0.9),
        ("This broadcast frame shows fans in the stands waving towels", 0.9),
    ])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, similarity=0.6
    ).recognize(MEDIA)
    assert len(cues) == 2, [c.text for c in cues]
    # And the marker still reaches the emitted text.
    assert all(c.text.startswith(CAPTION_PREFIX) for c in cues)
