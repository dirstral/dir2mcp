"""One recognizer's fault must degrade the request, not sink it (#945).

Measured on the pilot: a full-game recognition ran four GPU-hours (scorebug,
face, then 6,118 captioned frames) and ended as an unlogged 502 because one
recognizer raised late. Every cue the other recognizers had produced was
discarded with it, and the traceback existed nowhere: serve.py mapped the
exception to a 502 body without logging, and dir2mcp deliberately does not
echo backend bodies. These tests pin the three layers that now make a fault
survivable and visible.
"""

import json
import logging
import urllib.request
from pathlib import Path

import pytest

from dirstral_annotator import pipeline as pipeline_mod
from dirstral_annotator.model import Cue
from dirstral_annotator.recognizers import caption as caption_mod
from dirstral_annotator.recognizers import news as news_mod
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.caption import SceneCaptionRecognizer
from dirstral_annotator.roster import Roster
from dirstral_annotator.serve import serve

MEDIA = Path("game.mp4")


@pytest.fixture
def frames(monkeypatch, tmp_path):
    """Nine fake frames at 1 fps, so batch_size=3 gives three batches."""
    paths = []
    for i in range(9):
        p = tmp_path / f"f{i}.jpg"
        p.write_text(str(i))
        paths.append((float(i), p))
    monkeypatch.setattr(caption_mod, "iter_frames", lambda *a, **k: iter(paths))
    return paths


# --- layer 1: the caption recognizer survives a failing batch -----------------

def test_a_failing_batch_is_skipped_and_the_run_keeps_its_other_captions(frames, caplog):
    calls = {"n": 0}

    def captioner(paths):
        calls["n"] += 1
        if calls["n"] == 2:
            raise RuntimeError("CUDA error: device-side assert")
        # Distinct text per frame, so runs do not collapse across frames and
        # each surviving frame is identifiable in the output.
        return [(f"Scene at {p.name}: something distinct {p.name}", 0.9) for p in paths]

    with caplog.at_level(logging.WARNING):
        cues = SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=3).recognize(MEDIA)
    # Batches 1 and 3 captioned (frames f0-f2, f6-f8); batch 2 (f3-f5) is missing.
    texts = " ".join(c.text for c in cues)
    for kept in ("f0.jpg", "f1.jpg", "f2.jpg", "f6.jpg", "f7.jpg", "f8.jpg"):
        assert kept in texts, f"surviving frame {kept} lost its caption"
    for lost in ("f3.jpg", "f4.jpg", "f5.jpg"):
        assert lost not in texts, f"frame {lost} of the failed batch leaked into cues"
    assert "caption batch 2" in caplog.text and "RuntimeError" in caplog.text
    assert "1 of 3 batches failed" in caplog.text


def test_every_batch_failing_is_unavailability_not_zero_captions(frames):
    def captioner(paths):
        raise RuntimeError("backend gone")

    with pytest.raises(RecognizerUnavailable) as exc:
        SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=3).recognize(MEDIA)
    assert "all 3 batches" in str(exc.value) and "RuntimeError" in str(exc.value)


def test_a_contract_violation_still_raises_immediately(frames):
    # Wrong result count is not a transient, it is a broken backend; the
    # per-batch tolerance must not swallow it into "skipped".
    def captioner(paths):
        return [("one", 0.9)]  # for a batch of three

    with pytest.raises(RecognizerUnavailable) as exc:
        SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=3).recognize(MEDIA)
    assert "contract" in str(exc.value)


# --- layer 2: the pipeline keeps the other recognizers' cues -------------------

def test_one_recognizer_raising_does_not_discard_the_others(monkeypatch, tmp_path, caplog):
    class NewsOK:
        def __init__(self, **kw):
            pass

        def recognize(self, media):
            return [Cue(source="news", start_s=1.0, end_s=2.0, event="overlay_text",
                        entity_ids=(), confidence=0.9, text="BREAKING")]

    class CaptionBoom:
        def __init__(self, **kw):
            pass

        def recognize(self, media):
            raise RuntimeError("late explosion after four hours")

    monkeypatch.setattr(news_mod, "NewsOverlayRecognizer", NewsOK)
    monkeypatch.setattr(caption_mod, "SceneCaptionRecognizer", CaptionBoom)
    pipe = pipeline_mod.Pipeline(roster=Roster([]), games={}, news=True,
                                 caption_fn=lambda paths: [])
    with caplog.at_level(logging.ERROR):
        cues = pipe.cues_for(tmp_path / "game.mp4")
    assert [c.source for c in cues] == ["news"], "the healthy recognizer's cues must survive"
    assert pipe.skipped == ["caption: RuntimeError: late explosion after four hours"]
    # The traceback is in the LOG, not in the request.
    assert "recognizer caption failed" in caplog.text
    assert "Traceback" in caplog.text


def test_unavailability_still_skips_without_a_traceback(monkeypatch, tmp_path, caplog):
    class CaptionUnavailable:
        def __init__(self, **kw):
            pass

        def recognize(self, media):
            raise RecognizerUnavailable("no vision backend")

    monkeypatch.setattr(caption_mod, "SceneCaptionRecognizer", CaptionUnavailable)
    pipe = pipeline_mod.Pipeline(roster=Roster([]), games={}, caption_fn=lambda paths: [])
    with caplog.at_level(logging.ERROR):
        pipe.cues_for(tmp_path / "game.mp4")
    assert pipe.skipped == ["no vision backend"]
    assert "Traceback" not in caplog.text, "expected unavailability is not an error"


# --- layer 3: serve logs what dir2mcp deliberately will not echo ---------------

class _Stub:
    roster = Roster([])

    def __init__(self, raise_exc=None, skipped=()):
        self._raise = raise_exc
        self.skipped = list(skipped)

    def annotations_for(self, media):
        if self._raise:
            raise self._raise
        return []


def _post(base, media):
    req = urllib.request.Request(base + "/recognize", data=json.dumps({"path": str(media)}).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


def test_a_502_is_logged_with_its_traceback(tmp_path, caplog):
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x")
    server = serve(_Stub(raise_exc=RuntimeError("fuse blew up")), host="127.0.0.1", port=0)
    import threading
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        with caplog.at_level(logging.ERROR):
            status, body = _post(f"http://127.0.0.1:{server.server_address[1]}", media)
    finally:
        server.shutdown()
    assert status == 502 and "fuse blew up" in body["error"]
    assert "recognize game.mp4 failed" in caplog.text and "Traceback" in caplog.text


def test_a_degraded_200_logs_the_skip_reasons(tmp_path, caplog):
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x")
    server = serve(_Stub(skipped=["caption: RuntimeError: boom", "face: no bank"]),
                   host="127.0.0.1", port=0)
    import threading
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        with caplog.at_level(logging.WARNING):
            status, body = _post(f"http://127.0.0.1:{server.server_address[1]}", media)
    finally:
        server.shutdown()
    assert status == 200 and body["annotations"] == []
    assert "2 recognizer(s) skipped" in caplog.text and "caption: RuntimeError: boom" in caplog.text
