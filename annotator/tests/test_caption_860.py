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


def test_the_floor_runs_alongside_the_windows(fake_frames):
    """#860's tier B. Aiming alone is an EXCLUSIVE filter, and the measurement
    says that loses the capability: 54 of the 84 plays are invisible to the
    aimed tier, and the one fan cutaway in the game sits in dead time BETWEEN
    plays, which is where a broadcast puts human-interest shots. So the floor
    must reach frames no window selected.
    """
    captions = [("the pitcher throws from the mound", 0.9)] * 12
    # The moment nobody aimed at: a fan cutaway in dead time.
    captions[8] = ("a man on a yellow bodyboard in the water", 0.9)
    captioner, seen = fake_frames(captions)

    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, windows=[(1.0, 2.0)], floor_fps=0.25
    ).recognize(MEDIA)

    events = [c.event for c in cues]
    assert SCENE_OTHER in events, (
        "the floor never reached the cutaway at t=8; aiming stayed exclusive: %r" % events
    )
    # And the floor is a FLOOR, not a second full pass: far fewer than 12.
    assert sum(seen["batches"]) < 12


def test_a_frame_in_both_tiers_is_captioned_once(fake_frames):
    """The tiers overlap by construction, so selection dedupes by timestamp
    rather than paying the model twice for one frame."""
    captioner, seen = fake_frames([("the pitcher throws from the mound", 0.9)] * 4)
    SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, windows=[(0.0, 3.0)], floor_fps=1.0
    ).recognize(MEDIA)
    assert sum(seen["batches"]) == 4, "a frame in both tiers was captioned twice"


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
    # No floor here, so this pins the AIMED tier in isolation: with floor_fps
    # unset the windows select alone, which is what makes the cost saving real.
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


# --- #923: the claim gate ---------------------------------------------------
#
# The 400-frame labelled run said the caption is mostly right and one clause
# inside it is not, so a whole-caption confidence cannot isolate the failure
# (AUC 0.514) while asking the claim directly can (AUC 0.991). These pin the
# gate that follows from that: it guards the FILTERABLE event, it is activated
# by the presence of a backend rather than a flag, and it fails toward saying
# less rather than more.


def _prober(score, seen=None):
    """A ProbeFn returning a fixed P(yes), recording the questions it was asked."""

    def probe(frames, question):
        if seen is not None:
            seen.append(question)
        return [score] * len(frames)

    return probe


def test_923_an_unsupported_crowd_claim_is_withdrawn(fake_frames):
    """The measured failure: a static seated crowd described as cheering."""
    captioner, _ = fake_frames([("The crowd erupts in the stands, fans cheering wildly.", 0.9)])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(0.01)
    ).recognize(MEDIA)
    assert len(cues) == 1
    # The filterable claim is gone, so "show me the crowd reaction" cannot
    # return this moment.
    assert cues[0].event == SCENE_OTHER
    # The prose survives. The gate withdraws the claim, not the description.
    assert "crowd erupts" in cues[0].text


def test_923_a_supported_crowd_claim_survives(fake_frames):
    captioner, _ = fake_frames([("The crowd erupts in the stands, fans cheering wildly.", 0.9)])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(1.0)
    ).recognize(MEDIA)
    assert cues[0].event == SCENE_CROWD


def test_923_a_supported_celebration_survives(fake_frames):
    captioner, _ = fake_frames([("Teammates celebrate with high fives near the dugout.", 0.9)])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(1.0)
    ).recognize(MEDIA)
    assert cues[0].event == SCENE_CELEBRATION


def test_923_the_gate_is_off_without_a_prober(fake_frames):
    """Capability-driven: no backend, no gate, and the old behaviour intact."""
    captioner, _ = fake_frames([("The crowd erupts in the stands, fans cheering wildly.", 0.9)])
    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0).recognize(MEDIA)
    assert cues[0].event == SCENE_CROWD


def test_923_describing_events_are_never_probed(fake_frames):
    """scene_field measured ~0.94, so gating it would only cost recall."""
    asked = []
    captioner, _ = fake_frames([("The pitcher delivers to the batter from the mound.", 0.9)])
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(0.0, asked)
    ).recognize(MEDIA)
    assert cues[0].event == SCENE_FIELD
    assert asked == [], "a describing event must not cost a probe call"


def test_923_each_claim_asks_its_own_question(fake_frames):
    """A shared question would gate one claim with another claim's evidence."""
    asked = []
    captioner, _ = fake_frames([("Teammates celebrate with high fives near the dugout.", 0.9)])
    SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(1.0, asked)
    ).recognize(MEDIA)
    assert asked and asked[0] == caption_mod.CLAIM_PROBES[SCENE_CELEBRATION]
    assert asked[0] != caption_mod.CLAIM_PROBES[SCENE_CROWD]


def test_923_the_threshold_is_severe_by_default(fake_frames):
    """0.99, not 0.5: a wrong span is worse than a missing one in an archive."""
    captioner, _ = fake_frames([("The crowd erupts in the stands, fans cheering wildly.", 0.9)])
    # Comfortably "probably yes", and still not enough.
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(0.90)
    ).recognize(MEDIA)
    assert cues[0].event == SCENE_OTHER
    # An operator who wants recall can say so.
    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=_prober(0.90), claim_threshold=0.85
    ).recognize(MEDIA)
    assert cues[0].event == SCENE_CROWD


def test_923_a_run_passes_on_its_strongest_frame(fake_frames):
    """A celebration filling two frames of a longer run is still a celebration."""
    captioner, _ = fake_frames(
        [("Teammates celebrate with high fives near the dugout.", 0.9)] * 4
    )
    scores = [0.0, 0.0, 1.0, 0.0]

    def probe(frames, question):
        return scores[: len(frames)]

    cues = SceneCaptionRecognizer(
        captioner=captioner, fps=1.0, prober=probe
    ).recognize(MEDIA)
    assert len(cues) == 1, "identical captions should collapse into one run"
    assert cues[0].event == SCENE_CELEBRATION


def test_923_a_probe_that_breaks_its_contract_is_reported(fake_frames):
    """Silently trusting a short result would gate a run on another run's score."""
    captioner, _ = fake_frames([("The crowd erupts in the stands, fans cheering wildly.", 0.9)])

    def short(frames, question):
        return []

    with pytest.raises(RecognizerUnavailable, match="one P\\(yes\\) per frame"):
        SceneCaptionRecognizer(
            captioner=captioner, fps=1.0, prober=short
        ).recognize(MEDIA)


def test_923_gated_events_stay_inside_the_closed_vocabulary():
    """The gate must not invent a value the section 6.2 filter cannot serve."""
    assert set(caption_mod.CLAIM_PROBES) <= set(SCENE_EVENTS)
    assert SCENE_OTHER in SCENE_EVENTS
