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
from dirstral_annotator.recognizers.base import SCRUBBED_ERROR_MAX_CHARS, RecognizerUnavailable, scrub_error
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
    # The traceback frames are in the LOG, not in the request.
    assert "recognizer caption failed" in caplog.text
    assert "test_recognizer_resilience_945.py" in caplog.text


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
    assert "failed on" not in caplog.text, "expected unavailability is not an error"


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
    # The body is a stable public code with NO exception text (CWE-209); the
    # reason lives in this process's log, frames intact.
    assert status == 502
    assert body == {"error": "recognition failed", "code": "RECOGNITION_FAILED"}
    assert "recognize game.mp4 failed" in caplog.text
    assert "RuntimeError: fuse blew up" in caplog.text
    assert "test_recognizer_resilience_945.py" in caplog.text, "frames must survive scrubbing"


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


# --- CWE-209: what an upstream exception may carry must not reach a log, a
# skip reason or a response body (review finding on #946) ------------------

SENTINEL_KEY = "api_key=SENTINEL_SECRET_ABC123XYZ"
SENTINEL_BEARER = "Authorization: Bearer sk-SENTINELTOKEN-9f8e7d6c5b4a"
LEAKY = RuntimeError(
    f"POST https://vlm.example/v1/caption?{SENTINEL_KEY} -> 401; {SENTINEL_BEARER}; "
    "body: \x00\x01binary\x02fragment"
)


def test_scrub_error_redacts_credentials_urls_and_binary_but_keeps_the_type():
    out = scrub_error(LEAKY)
    assert out.startswith("RuntimeError: ")
    for secret in ("SENTINEL_SECRET_ABC123XYZ", "sk-SENTINELTOKEN"):
        assert secret not in out
    # The api_key pair sat INSIDE the URL query, and the query scrub swallows
    # the whole query (more redaction, not less), so it is asserted on a bare
    # message where it is the only thing to redact.
    assert scrub_error(RuntimeError("api_key=SENTINELX123 rejected")) == "RuntimeError: api_key=<redacted> rejected"
    assert "Authorization=<redacted>" in out
    assert "https://vlm.example/v1/caption?<redacted>" in out, "host kept, query dropped"
    # The two shapes the first draft missed: a bare scheme word, and a short
    # prefixed key under the opaque-token length floor.
    assert "sk-" not in scrub_error(RuntimeError("Bearer sk-abc12345defgh"))
    assert scrub_error(RuntimeError("key sk-abcdefgh1234")) == "RuntimeError: key <redacted>"
    assert "\x00" not in out and "\x01" not in out
    # Userinfo credentials sit BEFORE the host, where the query scrub cannot
    # reach them (second-round finding, CWE-532): the password goes, the host
    # and path stay so the log still says which endpoint failed.
    userinfo = RuntimeError("GET https://svc:SENTINEL_PW_77@vlm.example/v1/caption?k=SENTINEL_Q -> 407")
    got = scrub_error(userinfo)
    assert "SENTINEL_PW_77" not in got and "svc:" not in got
    assert "SENTINEL_Q" not in got
    assert got == "RuntimeError: GET https://<redacted>@vlm.example/v1/caption?<redacted> -> 407"
    # A URL without userinfo is untouched by the userinfo scrub: the "@" in a
    # path or query segment must not be mistaken for a credential boundary.
    plain = scrub_error(RuntimeError("GET https://vlm.example/v1/users/@me -> 404"))
    assert plain == "RuntimeError: GET https://vlm.example/v1/users/@me -> 404"
    long = RuntimeError("x" * 5000)
    assert len(scrub_error(long)) <= len("RuntimeError: ") + SCRUBBED_ERROR_MAX_CHARS


def test_a_secret_in_a_batch_failure_never_reaches_the_log(frames, caplog):
    calls = {"n": 0}

    def captioner(paths):
        calls["n"] += 1
        if calls["n"] == 2:
            raise LEAKY
        return [(f"Scene {p.name}", 0.9) for p in paths]

    with caplog.at_level(logging.WARNING):
        SceneCaptionRecognizer(captioner=captioner, fps=1.0, batch_size=3).recognize(MEDIA)
    assert "SENTINEL" not in caplog.text
    assert "RuntimeError" in caplog.text


def test_a_secret_in_a_recognizer_fault_never_reaches_skip_reasons_or_log(monkeypatch, tmp_path, caplog):
    class Boom:
        def __init__(self, **kw):
            pass

        def recognize(self, media):
            raise LEAKY

    monkeypatch.setattr(caption_mod, "SceneCaptionRecognizer", Boom)
    pipe = pipeline_mod.Pipeline(roster=Roster([]), games={}, caption_fn=lambda paths: [])
    with caplog.at_level(logging.ERROR):
        pipe.cues_for(tmp_path / "game.mp4")
    assert "SENTINEL" not in " ".join(pipe.skipped)
    assert "SENTINEL" not in caplog.text
    assert pipe.skipped and pipe.skipped[0].startswith("caption: RuntimeError:")


def test_a_secret_in_a_pipeline_fault_never_reaches_the_502_body_or_log(tmp_path, caplog):
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x")
    server = serve(_Stub(raise_exc=LEAKY), host="127.0.0.1", port=0)
    import threading
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with caplog.at_level(logging.ERROR):
            status, body = _post(f"http://127.0.0.1:{server.server_address[1]}", media)
    finally:
        server.shutdown()
    assert status == 502
    assert "SENTINEL" not in json.dumps(body) and "SENTINEL" not in caplog.text
    assert body["code"] == "RECOGNITION_FAILED"
