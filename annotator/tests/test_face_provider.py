"""Execution-provider selection for face recognition.

The pilot ran a full eval on CPU while two GPUs sat idle, because the provider
was hardcoded and nothing reported it. These pin the selection rules and, more
importantly, that a silent CPU fallback is visible.
"""

from __future__ import annotations

import logging

import pytest

from dirstral_annotator.recognizers import faces
from dirstral_annotator.recognizers.base import RecognizerUnavailable


def _offer(monkeypatch, providers):
    import sys, types
    mod = types.ModuleType("onnxruntime")
    mod.get_available_providers = lambda: list(providers)
    monkeypatch.setitem(sys.modules, "onnxruntime", mod)


def test_auto_prefers_cuda_when_offered(monkeypatch):
    monkeypatch.delenv(faces.PROVIDER_ENV, raising=False)
    _offer(monkeypatch, ["CUDAExecutionProvider", "CPUExecutionProvider"])
    providers, ctx = faces.select_face_providers()
    assert providers[0] == "CUDAExecutionProvider"
    assert ctx == 0
    assert providers[-1] == "CPUExecutionProvider", "CPU must remain a fallback"


def test_auto_falls_back_to_cpu(monkeypatch):
    monkeypatch.delenv(faces.PROVIDER_ENV, raising=False)
    _offer(monkeypatch, ["AzureExecutionProvider", "CPUExecutionProvider"])
    assert faces.select_face_providers() == (["CPUExecutionProvider"], -1)


def test_explicit_cpu_wins_over_an_available_gpu(monkeypatch):
    """An operator keeping a shared GPU free must be able to say so."""
    _offer(monkeypatch, ["CUDAExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setenv(faces.PROVIDER_ENV, "cpu")
    assert faces.select_face_providers() == (["CPUExecutionProvider"], -1)
    assert faces.select_face_providers("cpu") == (["CPUExecutionProvider"], -1)


def test_explicit_cuda_fails_loudly_when_unavailable(monkeypatch):
    """Asking for CUDA on a host that cannot provide it must raise, not quietly
    cost hours; that silent degradation is what this whole change is about."""
    _offer(monkeypatch, ["CPUExecutionProvider"])
    monkeypatch.setenv(faces.PROVIDER_ENV, "cuda")
    with pytest.raises(RecognizerUnavailable) as exc:
        faces.select_face_providers()
    assert "onnxruntime-gpu" in str(exc.value)


def test_missing_onnxruntime_degrades_to_cpu(monkeypatch):
    import sys
    monkeypatch.delenv(faces.PROVIDER_ENV, raising=False)
    monkeypatch.setitem(sys.modules, "onnxruntime", None)
    monkeypatch.setattr(faces, "select_face_providers", faces.select_face_providers)
    import builtins
    real = builtins.__import__

    def no_ort(name, *a, **kw):
        if name == "onnxruntime":
            raise ImportError("no onnxruntime")
        return real(name, *a, **kw)

    monkeypatch.setattr(builtins, "__import__", no_ort)
    assert faces.select_face_providers() == (["CPUExecutionProvider"], -1)


def test_session_providers_never_raises():
    """Diagnostics must not be able to break a run."""
    assert faces._session_providers(object()) == []

    class Broken:
        @property
        def models(self):
            raise RuntimeError("boom")

    assert faces._session_providers(Broken()) == []


@pytest.mark.parametrize("bad", ["gpu", "none", "CUDA11", "cude"])
def test_unknown_provider_is_rejected(monkeypatch, bad):
    """A typo must not quietly mean auto. That silent-default behaviour is the
    exact failure this module exists to remove."""
    _offer(monkeypatch, ["CUDAExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setenv(faces.PROVIDER_ENV, bad)
    with pytest.raises(RecognizerUnavailable) as exc:
        faces.select_face_providers()
    assert "unknown face provider" in str(exc.value)


@pytest.mark.parametrize("value", ["", "  ", "AUTO", "auto ", " CPU "])
def test_blank_and_cased_values_are_accepted(monkeypatch, value):
    """An empty variable means unset, and case/whitespace are normalised; only
    genuinely unrecognised words are rejected."""
    _offer(monkeypatch, ["CUDAExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setenv(faces.PROVIDER_ENV, value)
    providers, _ = faces.select_face_providers()
    expected = "CPUExecutionProvider" if value.strip().lower() == "cpu" else "CUDAExecutionProvider"
    assert providers[0] == expected


def test_session_providers_prefers_recognition():
    """Models can bind differently; reporting the first one found could claim
    CUDA while the model whose cost dominates ran on CPU."""
    class Sess:
        def __init__(self, provs): self._p = provs
        def get_providers(self): return list(self._p)

    class Model:
        def __init__(self, provs): self.session = Sess(provs)

    class App:
        models = {
            "detection": Model(["CUDAExecutionProvider"]),
            "recognition": Model(["CPUExecutionProvider"]),
        }

    assert faces._session_providers(App()) == ["CPUExecutionProvider"]


def test_session_providers_warns_on_disagreement(caplog):
    class Sess:
        def __init__(self, provs): self._p = provs
        def get_providers(self): return list(self._p)

    class Model:
        def __init__(self, provs): self.session = Sess(provs)

    class App:
        models = {
            "detection": Model(["CUDAExecutionProvider"]),
            "recognition": Model(["CPUExecutionProvider"]),
        }

    with caplog.at_level(logging.WARNING):
        faces._session_providers(App())
    assert any("different execution providers" in r.message for r in caplog.records)
