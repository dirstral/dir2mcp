"""The served shot detector (SPEC §8.10.3, spec 0.103.0).

Stdlib-only: the windowing and the scene rule are tested with a fake
predictor, and the /shots route with a stub detector, so no torch is needed.
The real ffmpeg decode is tested when ffmpeg is on PATH.
"""

import json
import shutil
import subprocess
import threading
import urllib.error
import urllib.request
from pathlib import Path

import jsonschema
import pytest

from dirstral_annotator.pipeline import Pipeline
from dirstral_annotator.roster import Roster
from dirstral_annotator.serve import serve
from dirstral_annotator.shots import (
    FRAME_BYTES, KEEP, LEAD, WINDOW, iter_frames, predict_stream, predictions_to_cuts,
)


def _frames(n):
    """Frame i carries its own index, so a predictor can report which frame it saw."""
    return [i.to_bytes(4, "big") * (FRAME_BYTES // 4) for i in range(n)]


def _index_predictor(calls):
    def predict(window):
        assert len(window) == WINDOW
        calls.append(1)
        return [float(int.from_bytes(f[:4], "big")) for f in window]
    return predict


@pytest.mark.parametrize("n", [1, 49, 50, 51, 99, 100, 101, 250, 1234])
def test_every_frame_gets_its_own_prediction(n):
    calls = []
    got = predict_stream(_frames(n), _index_predictor(calls))
    assert got == [float(i) for i in range(n)]
    # One window per 50 frames: the stream never holds more than one window.
    assert len(calls) == -(-n // KEEP)


def test_empty_stream():
    assert predict_stream([], _index_predictor([])) == []


def test_window_geometry_matches_the_reference_code():
    assert (WINDOW, KEEP, LEAD) == (100, 50, 25)


def test_predictions_to_cuts_scene_rule():
    # A transition at frames 10-11 starts shot 2 at frame 12; one at frame 30
    # starts shot 3 at frame 31. A transition still open at the end is no cut.
    p = [0.0] * 50
    p[10] = p[11] = 0.9
    p[30] = 0.7
    p[48] = p[49] = 0.8
    assert predictions_to_cuts(p, threshold=0.5, fps=25.0) == [0.48, 1.24]
    assert predictions_to_cuts([0.9, 0.1], threshold=0.5, fps=25.0) == [0.04]
    assert predictions_to_cuts([0.1] * 10) == []


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_iter_frames_decodes_at_a_fixed_rate(tmp_path):
    video = tmp_path / "two_seconds.mp4"
    subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
                    "-i", "color=c=red:s=320x240:d=2:r=30", "-pix_fmt", "yuv420p", str(video)],
                   check=True)
    frames = list(iter_frames(str(video)))
    assert len(frames) == 50  # 2 s at the fixed 25 fps, whatever the source rate
    assert all(len(f) == FRAME_BYTES for f in frames)


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_iter_frames_fails_loudly(tmp_path):
    bad = tmp_path / "not_a_video.mp4"
    bad.write_bytes(b"nope")
    with pytest.raises(RuntimeError, match="ffmpeg exited"):
        list(iter_frames(str(bad)))


class _StubShots:
    def __init__(self, cuts=None, fail=False):
        self._cuts, self._fail = cuts or [], fail

    def identity(self):
        return {"name": "stub", "version": "1"}

    def cuts(self, path):
        if self._fail:
            raise RuntimeError(f"model crashed reading {path}")
        return self._cuts


def _post(port, route, body):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{route}", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read())


def _serve(shots):
    server = serve(Pipeline(roster=Roster([]), games={}), host="127.0.0.1", port=0, shots=shots)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def _spec_schema():
    root = Path(__file__).resolve().parents[2]
    path = root / "dirstral-spec" / "spec" / "shots" / "response.schema.json"
    return json.loads(path.read_text()) if path.is_file() else None


def test_shots_route_answers_with_the_wire_shape(tmp_path):
    media = tmp_path / "news.mp4"
    media.write_bytes(b"\x00")
    server = _serve(_StubShots(cuts=[2.04, 6.12]))
    try:
        status, body = _post(server.server_address[1], "/shots", {"path": str(media)})
    finally:
        server.shutdown()
    assert status == 200
    assert body == {"detector": {"name": "stub", "version": "1"}, "cuts": [2.04, 6.12]}
    schema = _spec_schema()
    if schema is not None:
        jsonschema.validate(body, schema)


def test_shots_route_failure_is_a_502_without_exception_text(tmp_path):
    media = tmp_path / "news.mp4"
    media.write_bytes(b"\x00")
    server = _serve(_StubShots(fail=True))
    try:
        status, body = _post(server.server_address[1], "/shots", {"path": str(media)})
    finally:
        server.shutdown()
    assert status == 502
    assert body == {"error": "shot detection failed", "code": "SHOTS_FAILED"}
    assert str(tmp_path) not in json.dumps(body)


def test_shots_route_is_404_without_a_detector(tmp_path):
    media = tmp_path / "news.mp4"
    media.write_bytes(b"\x00")
    server = _serve(None)
    try:
        status, _ = _post(server.server_address[1], "/shots", {"path": str(media)})
    finally:
        server.shutdown()
    assert status == 404
