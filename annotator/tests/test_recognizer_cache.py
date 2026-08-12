"""A served pipeline builds each recognizer once, not once per request.

`Pipeline.cues_for` constructed every configured recognizer inside the
per-request call, and `serve` keeps one pipeline for the life of the process. So
every POST to /recognize reloaded the ONNX and YOLO models and re-embedded the
whole face bank. Measured with the real InsightFace backend and a 40 image bank:
46 seconds of construction, per request, before a single frame of the media was
read (#650).

There is a correctness edge under the performance one. Re-enrolling per request
means the bank on disk is re-read at an arbitrary moment, so two requests in one
run could answer from different galleries with nothing recording that they had.
Building once fixes both: the gallery a process serves from is the one it
enrolled, and a change is picked up when the configuration changes, not when a
request happens to land.

Caching also means concurrent requests share the instance, so these tests pin
the thread contract too: one request at a time inside any one recognizer.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.request
from pathlib import Path

import pytest

from dirstral_annotator.model import Cue, Player
from dirstral_annotator.pipeline import Pipeline
from dirstral_annotator.recognizers import faces, jersey, news, scorebug
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.roster import Roster
from dirstral_annotator.serve import serve

PLAYER_ID = "player:webb-logan"

#: Every recognizer the pipeline constructs lazily, as
#: (module, class attribute, the Pipeline settings that switch it on).
RECOGNIZERS = [
    (scorebug, "ScorebugRecognizer", {"scorebug": True}),
    (jersey, "JerseyRecognizer", {"jersey": True}),
    (faces, "FaceRecognizer", {"faces_bank": Path("/bank-that-the-stub-never-reads")}),
    (news, "NewsOverlayRecognizer", {"news": True}),
]

ALL_ENABLED = {k: v for _, _, settings in RECOGNIZERS for k, v in settings.items()}

#: Class name -> the `source` tag its cues carry, which is also the pipeline's
#: cache slot name.
NAMES = {
    "ScorebugRecognizer": "scorebug",
    "JerseyRecognizer": "jersey",
    "FaceRecognizer": "face",
    "NewsOverlayRecognizer": "news",
}


@pytest.fixture
def roster() -> Roster:
    return Roster([Player(id=PLAYER_ID, name="Logan Webb", number="62")])


class _Stub:
    """A recognizer that emits one cue and records nothing but its own name."""

    def __init__(self, name: str):
        self.name = name

    def recognize(self, media_path: Path) -> list[Cue]:
        return [Cue(source=self.name, start_s=1.0, end_s=2.0, event="appearance",
                    entity_ids=(PLAYER_ID,), confidence=0.9)]


def _install(monkeypatch, *, unavailable=(), recognizer=None):
    """Replace every recognizer class with a constructor counter.

    Returns the per-name construction log. Names in `unavailable` refuse to be
    built, which is the case the pipeline is supposed to remember.
    """
    built: dict[str, int] = {}

    for module, attr, _settings in RECOGNIZERS:
        def factory(*args, _name=NAMES[attr], **kwargs):
            built[_name] = built.get(_name, 0) + 1
            if _name in unavailable:
                raise RecognizerUnavailable(f"{_name} backend is not installed")
            return (recognizer or _Stub)(_name)

        monkeypatch.setattr(module, attr, factory)
    return built


@pytest.fixture
def media(tmp_path) -> Path:
    path = tmp_path / "game7.mp4"
    path.write_bytes(b"stand-in; every recognizer here is a stub")
    return path


# --- the headline contract --------------------------------------------------

def test_two_requests_build_each_recognizer_once(monkeypatch, roster, media):
    built = _install(monkeypatch)
    pipeline = Pipeline(roster=roster, **ALL_ENABLED)

    first = pipeline.annotations_for(media)
    second = pipeline.annotations_for(media)

    assert built == {"scorebug": 1, "jersey": 1, "face": 1, "news": 1}, built
    assert first == second, "the cached recognizers stopped answering"
    assert first, "the stubs must produce annotations, or this proves nothing"


def test_two_served_requests_build_each_recognizer_once(monkeypatch, roster, media):
    """The same contract through the surface #650 is about: a long lived
    ThreadingHTTPServer answering two POSTs to /recognize."""
    built = _install(monkeypatch)
    pipeline = Pipeline(roster=roster, **ALL_ENABLED)
    server = serve(pipeline, host="127.0.0.1", port=0)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_address[1]}/recognize"
        for _ in range(2):
            request = urllib.request.Request(
                url, data=json.dumps({"path": str(media)}).encode(),
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(request) as response:
                assert response.status == 200
                assert json.load(response)["annotations"]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)

    assert built == {"scorebug": 1, "jersey": 1, "face": 1, "news": 1}, built


# --- unavailability is remembered, not retried ------------------------------

def test_an_unavailable_recognizer_is_built_once_and_reported_every_time(
    monkeypatch, roster, media
):
    built = _install(monkeypatch, unavailable={"jersey"})
    pipeline = Pipeline(roster=roster, **ALL_ENABLED)

    pipeline.annotations_for(media)
    first_notes = list(pipeline.skipped)
    pipeline.annotations_for(media)
    second_notes = list(pipeline.skipped)

    assert built["jersey"] == 1, "a missing backend was probed again"
    # Remembering the failure must not stop reporting it: a caller that reads
    # `skipped` per request has to see the same answer every request.
    assert len(first_notes) == 1 and first_notes == second_notes
    assert "jersey" in first_notes[0]


def test_reconfiguration_is_not_hidden_by_the_cache(monkeypatch, roster, media):
    """Caching may not outlive the configuration it was built for.

    An operator who repoints the face bank, or turns a recognizer's parameters,
    must get a recognizer built from the new setting: for the face bank that is
    the correctness edge of this issue, because the gallery is the bank.
    """
    built = _install(monkeypatch, unavailable={"face"})
    pipeline = Pipeline(roster=roster, faces_bank=Path("/bank-a"))

    pipeline.annotations_for(media)
    assert built["face"] == 1
    assert pipeline.skipped

    pipeline.annotations_for(media)
    assert built["face"] == 1, "the same configuration was rebuilt"

    pipeline.faces_bank = Path("/bank-b")
    pipeline.annotations_for(media)
    assert built["face"] == 2, "the repointed bank reused the old recognizer"

    # And a parameter change counts as reconfiguration too.
    pipeline.fps = 2.0
    pipeline.annotations_for(media)
    assert built["face"] == 3


# --- the thread contract ---------------------------------------------------

def test_concurrent_requests_never_enter_one_recognizer_at_once(
    monkeypatch, roster, media
):
    """Sharing an instance is the point, so it has to be safe to share.

    These recognizers are not reentrant: NewsOverlayRecognizer.recognize clears
    and refills per-role reader state on itself, and the default jersey detector
    is an ultralytics model that is not documented thread safe. One lock per
    recognizer, so two requests queue instead of corrupting each other.
    """
    concurrency = {"inside": 0, "worst": 0}
    guard = threading.Lock()

    class _Reentrancy(_Stub):
        def recognize(self, media_path: Path) -> list[Cue]:
            with guard:
                concurrency["inside"] += 1
                concurrency["worst"] = max(concurrency["worst"],
                                           concurrency["inside"])
            time.sleep(0.05)  # long enough for a second thread to arrive
            with guard:
                concurrency["inside"] -= 1
            return super().recognize(media_path)

    built = _install(monkeypatch, recognizer=_Reentrancy)
    pipeline = Pipeline(roster=roster, scorebug=True)

    start = threading.Barrier(2)
    results: list[list] = []

    def request():
        start.wait(timeout=5)
        results.append(pipeline.annotations_for(media))

    threads = [threading.Thread(target=request) for _ in range(2)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=10)

    assert concurrency["worst"] == 1, "two requests were inside one recognizer"
    assert built == {"scorebug": 1}, "concurrent first requests each built a model"
    assert len(results) == 2 and all(results)


def test_concurrent_requests_keep_their_own_skip_notes(monkeypatch, roster, tmp_path):
    """`skipped` belongs to one request, not to the pipeline.

    A shared attribute fails two ways. Appending into one list merges the notes,
    and rebinding it per call still lets the last request to finish replace the
    answer a previous caller has not read yet. No list is interleaved and the
    caller still gets somebody else's notes.

    So every request here loses its recognizer for a reason that names its own
    media, and every request has to read back its own reason. The second barrier
    makes the replacement deterministic rather than a matter of timing: no
    request reads until all four have finished.
    """

    class _MissingEngine(_Stub):
        def recognize(self, media_path: Path) -> list[Cue]:
            raise RecognizerUnavailable(f"OCR engine missing for {media_path.name}")

    _install(monkeypatch, recognizer=_MissingEngine)
    pipeline = Pipeline(roster=roster, scorebug=True)

    clips = []
    for index in range(4):
        clip = tmp_path / f"clip-{index}.mp4"
        clip.write_bytes(b"stand-in; the stub never reads it")
        clips.append(clip)

    start = threading.Barrier(len(clips))
    settled = threading.Barrier(len(clips))
    seen: dict[str, list[str]] = {}
    guard = threading.Lock()

    def request(clip: Path):
        start.wait(timeout=5)
        pipeline.annotations_for(clip)
        settled.wait(timeout=5)
        notes = list(pipeline.skipped)
        with guard:
            seen[clip.name] = notes

    threads = [threading.Thread(target=request, args=(clip,)) for clip in clips]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=10)

    assert len(seen) == len(clips), f"a request never reported: {seen}"
    for name, notes in seen.items():
        assert notes == [f"OCR engine missing for {name}"], (name, notes)
