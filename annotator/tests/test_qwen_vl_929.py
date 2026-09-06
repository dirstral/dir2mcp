"""The Qwen3-VL backend behind the #923 claim gate (#929 deployment half).

The recognizer shipped with an injectable ProbeFn and CLAIM_THRESHOLD = 0.99,
measured on Qwen3-VL-4B reading P(yes) off the first answer token. Nothing in
the package could construct that backend, so the gate never ran anywhere.
These tests pin the arithmetic the threshold depends on (without a model), the
caption parsing, the unavailability contract, and that `serve --caption`
actually delivers a prober to the recognizer.
"""

import math
import sys
import types

import pytest

from dirstral_annotator import cli, pipeline as pipeline_mod
from dirstral_annotator.roster import Roster
from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers import qwen_vl
from dirstral_annotator.recognizers.base import RecognizerUnavailable


# --- P(yes): the exact computation the 400-frame calibration used ----------

def _lp_table(table):
    """A logprobs_of over a {spelling: probability} table; None if absent."""
    return lambda w: (math.log(table[w]) if w in table else None)


def test_yes_probability_is_normalized_over_yes_and_no_mass():
    p = qwen_vl.yes_probability(_lp_table({"yes": 0.6, " Yes": 0.2, "no": 0.2}))
    assert p == pytest.approx(0.8 / (0.8 + 0.2))


def test_spellings_that_are_not_single_tokens_are_skipped_not_guessed():
    # Only one yes spelling and one no spelling resolve; the rest return None.
    p = qwen_vl.yes_probability(_lp_table({"YES": 0.3, "NO": 0.1}))
    assert p == pytest.approx(0.75)


def test_no_mass_on_either_side_is_none_not_a_verdict():
    # The recognizer's contract turns None into unavailability. A 0 here would
    # suppress every claim and a 1 would publish every claim; both look like a
    # decision, and neither is one.
    assert qwen_vl.yes_probability(_lp_table({})) is None


def test_the_calibration_constants_are_the_measured_ones():
    # Changing any of these is a new instrument; the 0.99 threshold in
    # caption.py belongs to exactly this model, prompt and pixel budget.
    assert qwen_vl.DEFAULT_MODEL == "Qwen/Qwen3-VL-4B-Instruct"
    assert qwen_vl.MAX_PIXELS == 1280 * 720
    assert qwen_vl.CAPTION_PROMPT.startswith("Describe what this broadcast frame shows")
    assert "confidence:" in qwen_vl.CAPTION_PROMPT


# --- caption parsing -------------------------------------------------------

def test_caption_and_self_confidence_are_split():
    text, conf = qwen_vl._parse_caption(
        "Fans in the stands stand and cheer as a ball flies out.  \nconfidence: 0.95"
    )
    assert text == "Fans in the stands stand and cheer as a ball flies out."
    assert conf == 0.95


def test_missing_or_garbled_confidence_is_neutral_not_zero():
    # 0.5 neither clears nor fails a sane floor: the missing number is the
    # model's failure, not evidence about the frame.
    assert qwen_vl._parse_caption("A pitcher winds up.")[1] == 0.5
    assert qwen_vl._parse_caption("A pitcher winds up.\nconfidence: high")[1] == 0.5
    assert qwen_vl._parse_caption("A pitcher winds up.\nconfidence: 7")[1] == 0.5


# --- unavailability, capability-driven -------------------------------------

def test_missing_extra_is_unavailability_with_the_install_hint(monkeypatch):
    real_import = __import__

    def no_torch(name, *a, **k):
        if name in ("torch", "transformers", "PIL"):
            raise ImportError(name)
        return real_import(name, *a, **k)

    monkeypatch.setattr("builtins.__import__", no_torch)
    with pytest.raises(RecognizerUnavailable) as exc:
        qwen_vl.load_backend()
    assert "dirstral-annotator[caption]" in str(exc.value)


def test_cuda_requested_but_absent_is_unavailability_not_a_cpu_fallback(monkeypatch):
    # A CPU run of a three-hour game would take days and look like a hang, so
    # the backend refuses rather than degrading silently.
    fake_torch = types.SimpleNamespace(cuda=types.SimpleNamespace(is_available=lambda: False))
    monkeypatch.setitem(sys.modules, "torch", fake_torch)
    monkeypatch.setitem(sys.modules, "PIL", types.SimpleNamespace(Image=object()))
    monkeypatch.setitem(sys.modules, "transformers", types.SimpleNamespace(
        AutoModelForImageTextToText=object(), AutoProcessor=object()))
    with pytest.raises(RecognizerUnavailable) as exc:
        qwen_vl.load_backend(device="cuda:0")
    assert "no CUDA device" in str(exc.value)


# --- serve --caption reaches the recognizer WITH the prober -----------------

def test_serve_caption_delivers_captioner_and_prober(monkeypatch, tmp_path):
    captioner = lambda paths: [("Scene", 0.9) for _ in paths]  # noqa: E731
    prober = lambda paths, q: [0.995 for _ in paths]  # noqa: E731
    monkeypatch.setattr(qwen_vl, "load_backend", lambda **kw: (captioner, prober))

    built = {}

    class SpyRecognizer:
        def __init__(self, **kw):
            built.update(kw)

        def recognize(self, media):
            return []

    monkeypatch.setattr(caption_mod, "SceneCaptionRecognizer", SpyRecognizer)

    roster = tmp_path / "roster.json"
    roster.write_text("[]")
    args = cli.build_parser().parse_args([
        "serve", "--roster", str(roster), "--caption", "--caption-fps", "0.2",
    ])
    pipe = cli._pipeline(args, Roster([]), {})
    assert pipe.caption_fn is captioner
    assert pipe.probe_fn is prober
    assert pipe.caption_fps == 0.2

    pipe.cues_for(tmp_path / "game.mp4")
    # The gate is only on when the recognizer is handed the prober: a pipeline
    # that loaded the backend but dropped the prober would caption ungated
    # and look identical from the outside.
    assert built.get("captioner") is captioner
    assert built.get("prober") is prober


def test_serve_without_caption_flag_loads_nothing(monkeypatch, tmp_path):
    def must_not_load(**kw):
        raise AssertionError("load_backend called without --caption")

    monkeypatch.setattr(qwen_vl, "load_backend", must_not_load)
    roster = tmp_path / "roster.json"
    roster.write_text("[]")
    args = cli.build_parser().parse_args(["serve", "--roster", str(roster)])
    pipe = cli._pipeline(args, Roster([]), {})
    assert pipe.caption_fn is None and pipe.probe_fn is None


def test_unavailable_backend_degrades_with_a_reason_instead_of_crashing(monkeypatch, tmp_path, capsys):
    def unavailable(**kw):
        raise RecognizerUnavailable("no CUDA device")

    monkeypatch.setattr(qwen_vl, "load_backend", unavailable)
    roster = tmp_path / "roster.json"
    roster.write_text("[]")
    args = cli.build_parser().parse_args(["serve", "--roster", str(roster), "--caption"])
    pipe = cli._pipeline(args, Roster([]), {})
    assert pipe.caption_fn is None and pipe.probe_fn is None
    err = capsys.readouterr().err
    assert "--caption requested but unavailable" in err and "no CUDA device" in err


def test_a_new_prober_rebuilds_the_recognizer_rather_than_reusing_an_ungated_one(monkeypatch, tmp_path):
    builds = []

    class SpyRecognizer:
        def __init__(self, **kw):
            builds.append(kw.get("prober"))

        def recognize(self, media):
            return []

    monkeypatch.setattr(caption_mod, "SceneCaptionRecognizer", SpyRecognizer)
    captioner = lambda paths: []  # noqa: E731
    prober = lambda paths, q: []  # noqa: E731
    media = tmp_path / "game.mp4"

    # ONE pipeline, reconfigured between requests: the recognizer slot is
    # keyed per name, so if probe_fn were not part of the key the second
    # request would reuse the ungated instance and the gate would silently
    # stay off for the lifetime of the server.
    pipe = pipeline_mod.Pipeline(roster=Roster([]), games={}, caption_fn=captioner)
    pipe.cues_for(media)
    pipe.probe_fn = prober
    pipe.cues_for(media)
    assert builds == [None, prober]
