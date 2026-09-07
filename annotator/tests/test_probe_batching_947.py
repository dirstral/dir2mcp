"""The claim probe must respect batch_size (#947).

Measured on the pilot: the captioner was batched, the prober was not, and a
collapsed run of dozens of frames went to the backend in one forward pass --
a 3.38 GiB allocation on a GPU with 1.9 GiB free. batch_size exists to bound
peak memory; the probe path has to honour the same knob.
"""

import pytest

from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.caption import (
    CLAIM_PROBES,
    SCENE_CELEBRATION,
    SCENE_OTHER,
    SceneCaptionRecognizer,
)


def _long_run(monkeypatch, tmp_path, n):
    """n frames at 1 fps whose captions all collapse into ONE celebration run.

    Returns the captioner and the ordered frame paths, so a test can assert
    WHICH frames reached the prober and not only how many.
    """
    paths = []
    for i in range(n):
        p = tmp_path / f"f{i:03d}.jpg"
        p.write_text(str(i))
        paths.append((float(i), p))
    monkeypatch.setattr(caption_mod, "iter_frames", lambda *a, **k: iter(paths))
    text = "Players celebrate at home plate, high-fives all around"
    return lambda ps: [(text, 0.9) for _ in ps], [p for _, p in paths]


def test_probe_calls_never_exceed_batch_size(monkeypatch, tmp_path):
    captioner, expected = _long_run(monkeypatch, tmp_path, 20)
    seen = []

    def prober(frames, question):
        seen.append(list(frames))
        assert question == CLAIM_PROBES[SCENE_CELEBRATION]
        return [0.999 for _ in frames]

    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=8,
                                  prober=prober).recognize("game.mp4")
    assert seen, "the claim was never probed"
    sizes = [len(part) for part in seen]
    assert max(sizes) <= 8, f"a probe call exceeded batch_size: {sizes}"
    assert sizes == [8, 8, 4]
    # Sizes alone would pass if a slice were repeated and another dropped, so
    # the frames themselves are compared: every frame of the run, once, in
    # order (#948 review).
    submitted = [f for part in seen for f in part]
    assert submitted == expected, "the probe must see each frame of the run exactly once, in order"
    assert [c.event for c in cues] == [SCENE_CELEBRATION], "verdict must be unchanged by slicing"


def test_max_over_slices_keeps_the_strongest_frame_rule(monkeypatch, tmp_path):
    # Only the LAST slice carries a passing frame: the run must still pass,
    # exactly as it did when the run was probed whole.
    captioner, _ = _long_run(monkeypatch, tmp_path, 20)
    calls = {"n": 0}

    def prober(frames, question):
        calls["n"] += 1
        return [0.999 if (calls["n"] == 3 and i == 0) else 0.01 for i, _ in enumerate(frames)]

    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=8,
                                  prober=prober).recognize("game.mp4")
    assert [c.event for c in cues] == [SCENE_CELEBRATION]


def test_no_slice_passing_demotes_the_claim(monkeypatch, tmp_path):
    captioner, _ = _long_run(monkeypatch, tmp_path, 20)
    cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=8,
                                  prober=lambda fr, q: [0.01 for _ in fr]).recognize("game.mp4")
    assert [c.event for c in cues] == [SCENE_OTHER]


def test_slice_count_mismatch_is_still_a_contract_violation(monkeypatch, tmp_path):
    captioner, _ = _long_run(monkeypatch, tmp_path, 20)
    with pytest.raises(RecognizerUnavailable) as exc:
        SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=8,
                               prober=lambda fr, q: [0.5]).recognize("game.mp4")
    assert "contract is one P(yes) per frame" in str(exc.value)
