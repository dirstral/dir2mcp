"""Parallel per-frame reading must be invisible in the output.

The scorebug and jersey recognizers read frames on a worker pool. The whole
contract of that change is that it buys wall time and nothing else: the same
media must yield the same cue list, in the same order, at the same
confidences, whatever order the workers happen to finish in.

These tests attack that from both ends. They compare a serial run with a
parallel one on identical input, and they force the pathological completion
order (last frame first) that a naive implementation would leak into the cue
list. Backend-free: no tesseract, no Pillow, no video.

Note on what the completion-order tests can prove for each recognizer. The
jersey recognizer carries each frame's timestamp alongside its own result and
`collapse_sightings` sorts, so shuffling the order results come back in is
harmless there and pairing a result with the wrong timestamp is the failure
worth testing. The scorebug's region search is real sequential state, so for
it the order genuinely does matter and taking the wrong band's result is the
failure worth testing. Both are pinned below.
"""

from __future__ import annotations

import json
import threading
from concurrent.futures import Future

import pytest

from dirstral_annotator.recognizers import jersey, overlay, scorebug
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.jersey import JerseyRecognizer
from dirstral_annotator.recognizers.scorebug import ScorebugRecognizer, default_workers
from dirstral_annotator.roster import Roster

MEDIA = "game.mp4"
WHOLE = (0.0, 0.0, 1.0, 1.0)
WORKERS = 4


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:daylen-lile", "name": "Daylen Lile", "number": "4",
         "aliases": ["Lile"], "mlbam_id": 1},
        {"id": "player:robbie-ray", "name": "Robbie Ray", "number": "38",
         "aliases": ["Ray"], "mlbam_id": 2},
        {"id": "player:drew-gilbert", "name": "Drew Gilbert", "number": "0",
         "aliases": ["Gilbert"], "mlbam_id": 4},
        {"id": "player:jung-hoo-lee", "name": "Jung Hoo Lee", "number": "51",
         "aliases": ["Lee"], "mlbam_id": 5},
    ]))
    return Roster.load(path)


# A broadcast's worth of variety in a handful of frames: an at-bat that runs,
# a pitch graphic, a replay with no bug in it at all, a bug that only offers a
# loose read, and a second at-bat after the break.
SCRIPT = [
    "5.LILE 1-2 RAY P: 87 “6 @@ 2-0",
    "5.LILE 1-2 RAY P: 87 “6 @@ 2-0",
    "5.LILE 1-2 SLIDER 88 MPH “6 @@ 2-0",
    "5.LILE 2-2 RAY P: 88 “6 @@ 2-0",
    "Beir Lee GAS Pt atthe gL) TAT eM we",
    "",
    "a ray of light on the FRR ahe ey Maes",
    "6.GILBERT 0-0 RAY P: 91 “6 @@ 2-0",
    "6.GILBERT 0-1 FOURSEAM 92upeq “G @@",
    "6.GILBERT 0-1 RAY P: 92 “6 @@ 2-0",
] * 6


@pytest.fixture
def frames(monkeypatch, tmp_path):
    """Drive both recognizers off a scripted list of per-frame OCR strings."""

    def install(texts, fps=0.5):
        listing = []
        for i, text in enumerate(texts):
            path = tmp_path / f"frame-{i:05d}.jpg"
            path.write_text(text)
            listing.append((i / fps, path))

        # The scorebug samples and preprocesses through the shared overlay
        # reader; the jersey recognizer still does its own crop.
        for module in (overlay, jersey):
            monkeypatch.setattr(module, "iter_frames", lambda *a, **k: iter(listing))
        # The real crop needs Pillow and a real JPEG; the fakes hand the OCR
        # callable the frame itself.
        monkeypatch.setattr(
            overlay, "_prepared_crops", lambda frame, region, work: iter([frame])
        )
        # The fallback rendering of a band that missed, faked the same way: the
        # frame's own text is what the engine sees, whichever pass asked for it.
        monkeypatch.setattr(
            overlay, "_adaptive_crops", lambda frame, region, work: iter([frame])
        )
        monkeypatch.setattr(jersey, "_crop", lambda frame, bbox, work: frame)
        return lambda frame: frame.read_text()

    return install


class _ReverseExecutor:
    """Runs everything, but only hands back results in reverse submit order.

    A pool whose completion order is exactly the opposite of the frame order:
    the worst case for any implementation that lets completion order reach the
    cue list. Nothing runs until a result is awaited, and then the most
    recently submitted task runs first.
    """

    def __init__(self, max_workers=None, thread_name_prefix="", *args, **kwargs):
        self._queued: list[tuple[Future, object, tuple, dict]] = []

    def submit(self, fn, *args, **kwargs):
        future: Future = Future()
        self._queued.append((future, fn, args, kwargs))
        return future

    def _drain(self):
        while self._queued:
            future, fn, args, kwargs = self._queued.pop()  # newest first
            if not future.set_running_or_notify_cancel():
                continue
            try:
                future.set_result(fn(*args, **kwargs))
            except BaseException as exc:  # noqa: BLE001 - mirrors Executor
                future.set_exception(exc)

    def shutdown(self, wait=True, cancel_futures=False):
        if cancel_futures:
            self._queued.clear()


def _watching(ocr):
    """Wrap an OCR callable so the test can see which threads called it."""
    threads: set[str] = set()
    lock = threading.Lock()

    def watched(frame):
        with lock:
            threads.add(threading.current_thread().name)
        return ocr(frame)

    return watched, threads


def _reverse_executor_factory(monkeypatch, module):
    """Make `module` schedule on a reverse-completion pool.

    The scorebug's pool lives in `overlay` now, the jersey's still in `jersey`,
    so the module to patch differs; both look their factory up by module global
    at construction time.
    """
    pool = _ReverseExecutor()

    def new_executor(workers, name="test"):
        return pool

    monkeypatch.setattr(module, "new_executor", new_executor)

    original = Future.result

    def result(self, timeout=None):
        pool._drain()
        return original(self, timeout)

    monkeypatch.setattr(Future, "result", result)
    return pool


# --- worker count ----------------------------------------------------------

def test_worker_count_comes_from_the_environment(monkeypatch):
    monkeypatch.setenv(overlay.WORKERS_ENV, "3")
    assert default_workers() == 3


def test_one_worker_selects_the_serial_path(monkeypatch, tmp_path, roster, frames):
    monkeypatch.setenv(overlay.WORKERS_ENV, "1")
    assert default_workers() == 1
    rec = ScorebugRecognizer(roster, ocr=frames(SCRIPT), crop=WHOLE)
    assert rec.workers == 1
    assert isinstance(overlay._reader(rec.ocr, tmp_path, rec.workers),
                      overlay._SerialReader)
    assert isinstance(overlay._reader(rec.ocr, tmp_path, 2), overlay._PooledReader)


def test_recognizers_read_in_parallel_unless_told_otherwise(monkeypatch, roster, frames):
    """The pool is the default, not an opt-in: an unset environment on a
    multi-core host has to give more than one worker."""
    monkeypatch.delenv(overlay.WORKERS_ENV, raising=False)
    monkeypatch.setattr(overlay.os, "cpu_count", lambda: 16)
    assert default_workers() > 1
    assert ScorebugRecognizer(roster, ocr=frames(SCRIPT)).workers > 1
    assert JerseyRecognizer(
        roster, detector=lambda frame: [], ocr=frames(SCRIPT)
    ).workers > 1


def test_the_default_leaves_the_host_room_for_tesseracts_own_threads(monkeypatch):
    """A worker is not one core's worth of work: tesseract runs four threads
    per page, so N workers put 4N threads on the machine. The default has to
    scale with the host and stay capped."""
    monkeypatch.delenv(overlay.WORKERS_ENV, raising=False)
    for cores, expected in ((1, 1), (4, 1), (8, 2), (16, 4), (32, 8), (256, 8)):
        monkeypatch.setattr(overlay.os, "cpu_count", lambda cores=cores: cores)
        assert default_workers() == expected, cores
    # cpu_count() is documented as possibly None.
    monkeypatch.setattr(overlay.os, "cpu_count", lambda: None)
    assert default_workers() == 1


def test_a_nonsense_worker_count_falls_back_to_the_host(monkeypatch):
    for junk in ("", "  ", "many", "0", "-4", "2.5"):
        monkeypatch.setenv(overlay.WORKERS_ENV, junk)
        assert default_workers() >= 1
    monkeypatch.delenv(overlay.WORKERS_ENV)
    assert 1 <= default_workers() <= overlay.MAX_DEFAULT_WORKERS


# --- scorebug --------------------------------------------------------------

def test_scorebug_parallel_matches_serial(roster, frames):
    """The headline contract: same input, same cue list, in the same order."""
    serial = ScorebugRecognizer(roster, ocr=frames(SCRIPT), workers=1).recognize(MEDIA)
    parallel = ScorebugRecognizer(
        roster, ocr=frames(SCRIPT), workers=WORKERS
    ).recognize(MEDIA)
    assert serial == parallel
    assert serial, "the fixture must produce cues, or this proves nothing"


def test_scorebug_parallel_matches_serial_with_a_pinned_region(roster, frames):
    # The swept path locks onto a band part way through; the pinned one never
    # sweeps at all. Both have to survive the pool.
    serial = ScorebugRecognizer(
        roster, ocr=frames(SCRIPT), crop=WHOLE, workers=1
    ).recognize(MEDIA)
    parallel = ScorebugRecognizer(
        roster, ocr=frames(SCRIPT), crop=WHOLE, workers=WORKERS
    ).recognize(MEDIA)
    assert serial == parallel and serial


def test_scorebug_survives_reversed_completion_order(monkeypatch, roster, frames):
    serial = ScorebugRecognizer(roster, ocr=frames(SCRIPT), workers=1).recognize(MEDIA)
    _reverse_executor_factory(monkeypatch, overlay)
    reversed_run = ScorebugRecognizer(
        roster, ocr=frames(SCRIPT), workers=WORKERS
    ).recognize(MEDIA)
    assert reversed_run == serial and serial


def test_scorebug_parallel_matches_serial_across_a_lock_release(roster, frames):
    """The band lock is the sequential state the pool has to be guessed past.

    A long stretch with no bug in it releases the lock and puts the search
    back to sweeping, so the bands wanted for the frames already dispatched
    change under the pool. Getting that wrong is how a parallel run would
    quietly read the wrong crop and lock onto a different band.
    """
    quiet = overlay._RegionSearch.RELEASE_AFTER_MISSES + 10
    script = SCRIPT[:20] + ["Beir Lee GAS Pt atthe"] * quiet + SCRIPT[:20]
    serial = ScorebugRecognizer(roster, ocr=frames(script), workers=1)
    parallel = ScorebugRecognizer(roster, ocr=frames(script), workers=WORKERS)
    cues = serial.recognize(MEDIA)
    assert cues and cues == parallel.recognize(MEDIA)


def test_pooled_reader_reads_a_band_nobody_dispatched(roster, frames, tmp_path):
    """The loop dispatches what it is about to want, but `read` must not
    depend on that: a band it was never handed is read on the spot."""
    ocr = frames(SCRIPT)
    frame = tmp_path / "frame-00000.jpg"
    with overlay._PooledReader(ocr, tmp_path, WORKERS) as reader:
        texts = reader.read(7, frame, WHOLE)
    names, pitches, _ = scorebug.parse_bands(texts)
    assert scorebug.NameRead("LILE", scorebug.BATTER) in names
    assert [p.speed for p in pitches] == []


def test_scorebug_reads_every_frame_exactly_once_per_wanted_band(
    roster, frames, monkeypatch
):
    """A speculative dispatch that is not taken must not reach the cue list,
    and a band that is taken must not be OCR'd twice into it.

    The adaptive fallback is counted separately, and pinned separately below:
    it is a SECOND read of a band the shipped passes could not read, so mixing
    the two counts would stop this test seeing a band read twice by mistake.
    """
    base = frames(SCRIPT)
    seen: list[str] = []
    retried: list[str] = []
    lock = threading.Lock()

    def counting_ocr(frame):
        text = base(frame)
        with lock:
            seen.append(frame.name)
        return text

    def counting_fallback(ocr, frame, region, work):
        with lock:
            retried.append(frame.name)
        return []

    monkeypatch.setattr(overlay, "read_band_adaptive", counting_fallback)
    rec = ScorebugRecognizer(roster, ocr=counting_ocr, crop=WHOLE, workers=WORKERS)
    rec.recognize(MEDIA)
    # One band, one preprocessing pass per frame under the fake crop.
    assert sorted(seen) == sorted(f"frame-{i:05d}.jpg" for i in range(len(SCRIPT)))
    # The frames a scorebug WAS read off cost nothing extra, which is the whole
    # shape of the fallback: it is reached only where the shipped passes came
    # back with nothing, and a run it never helps stops paying for it.
    bug = {f"frame-{i:05d}.jpg" for i, text in enumerate(SCRIPT) if "LILE" in text}
    assert bug and not (bug & set(retried))
    # This script has replay frames with no bug on them, so the fallback is
    # reached; the point is that it stays inside its budget and never touches
    # a frame the reader already read.
    assert retried and len(retried) <= overlay.FALLBACK_TRIALS


def test_scorebug_reads_on_the_worker_pool(roster, frames):
    """Equality with a serial run is also what a silently serial pool would
    give, so pin that the reads really do leave the calling thread.

    MainThread is allowed because the fallback re-read runs there on purpose:
    whether a band needs it is only known after the caller has interpreted the
    ordinary read, which happens on the reading loop's own thread.
    """
    ocr, threads = _watching(frames(SCRIPT))
    ScorebugRecognizer(roster, ocr=ocr, crop=WHOLE, workers=WORKERS).recognize(MEDIA)
    pooled = {name for name in threads if name != "MainThread"}
    assert len(pooled) > 1, f"reads never left the calling thread: {threads}"
    assert all(name.startswith("scorebug") for name in pooled), threads


def test_scorebug_unavailable_backend_propagates_from_a_worker(roster, frames):
    """A worker that cannot read a frame must surface as an unavailable
    recognizer, which the pipeline skips, not as a broken pool."""
    frames(SCRIPT)

    def refuses(frame):
        raise RecognizerUnavailable("Pillow is required for scorebug OCR")

    rec = ScorebugRecognizer(roster, ocr=refuses, crop=WHOLE, workers=WORKERS)
    with pytest.raises(RecognizerUnavailable):
        rec.recognize(MEDIA)


# --- jersey ----------------------------------------------------------------

JERSEY_SCRIPT = ["4", "38", "", "51", "4", "0", "99", "38", "", "4"] * 6


@pytest.fixture
def jersey_parts(frames):
    """A detector that finds one player per frame and OCR that reads the
    frame's scripted number off it."""
    ocr = frames(JERSEY_SCRIPT)
    return (lambda frame: [(0, 0, 10, 10)]), ocr


def test_jersey_parallel_matches_serial(roster, jersey_parts):
    detect, ocr = jersey_parts
    serial = JerseyRecognizer(
        roster, detector=detect, ocr=ocr, workers=1
    ).recognize(MEDIA)
    parallel = JerseyRecognizer(
        roster, detector=detect, ocr=ocr, workers=WORKERS
    ).recognize(MEDIA)
    assert serial == parallel
    assert serial, "the fixture must produce cues, or this proves nothing"


def test_jersey_survives_reversed_completion_order(monkeypatch, roster, jersey_parts):
    detect, ocr = jersey_parts
    serial = JerseyRecognizer(
        roster, detector=detect, ocr=ocr, workers=1
    ).recognize(MEDIA)
    _reverse_executor_factory(monkeypatch, jersey)
    reversed_run = JerseyRecognizer(
        roster, detector=detect, ocr=ocr, workers=WORKERS
    ).recognize(MEDIA)
    assert reversed_run == serial and serial


def test_jersey_keeps_a_multi_player_frame_in_detection_order(roster, frames):
    """Several detections in one frame are still one frame: their numbers must
    reach the sighting list in the order the detector returned them."""
    ocr = frames(["4 38 51"] * 12)

    def detect(frame):
        return [(0, 0, 10, 10)]

    serial = JerseyRecognizer(roster, detector=detect, ocr=ocr, workers=1)
    parallel = JerseyRecognizer(roster, detector=detect, ocr=ocr, workers=WORKERS)
    assert serial.recognize(MEDIA) == parallel.recognize(MEDIA)


def test_jersey_reads_on_the_worker_pool(roster, jersey_parts):
    detect, base = jersey_parts
    ocr, threads = _watching(base)
    JerseyRecognizer(roster, detector=detect, ocr=ocr, workers=WORKERS).recognize(MEDIA)
    assert len(threads) > 1, f"reads never left the calling thread: {threads}"
    assert all(name.startswith("jersey") for name in threads), threads


def test_jersey_unavailable_backend_propagates_from_a_worker(roster, jersey_parts):
    detect, _ = jersey_parts

    def refuses(frame):
        raise RecognizerUnavailable("Pillow is required for jersey OCR")

    rec = JerseyRecognizer(roster, detector=detect, ocr=refuses, workers=WORKERS)
    with pytest.raises(RecognizerUnavailable):
        rec.recognize(MEDIA)


def test_jersey_builds_a_detector_per_worker_thread(roster, frames):
    """Ultralytics models are not thread safe, so the default adapter is
    rebuilt per worker rather than shared."""
    built: list[int] = []
    lock = threading.Lock()

    def factory():
        with lock:
            built.append(1)
        return lambda frame: [(0, 0, 10, 10)]

    detectors = jersey._Detectors(factory)
    threads = [threading.Thread(target=lambda: detectors.current()) for _ in range(3)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert len(built) == 3
    # A thread that asks twice keeps the one it was given.
    detectors.current()
    detectors.current()
    assert len(built) == 4


def test_jersey_hands_the_constructed_detector_to_the_first_worker():
    """The instance built in the constructor is used, not left idle beside a
    freshly built one."""

    def seed(frame):
        return []

    def factory():
        return lambda frame: []

    detectors = jersey._Detectors(factory, seed=seed)
    assert detectors.current() is seed



# --- a missing vision stack degrades the RUN, not just the adapter ----------

def _one_scripted_frame(monkeypatch, tmp_path):
    """Put a single real file on the reader's frame path.

    Frame extraction is ffmpeg's job and not what these assert, so it is
    replaced; what matters is that the reader reaches the OCR call at all.
    """
    from dirstral_annotator.recognizers import overlay

    frame = tmp_path / "frame-00000.jpg"
    frame.write_bytes(b"stand-in; decoding is patched out too")
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter([(0.0, frame)]))
    # Hand the frame straight to the OCR call. Rendering the two preprocessing
    # crops is Pillow's job and has its own contract test; patching it keeps
    # this test about the ENGINE being missing rather than about whether a
    # stand-in file happens to decode as a JPEG.
    monkeypatch.setattr(overlay, "_prepared_crops", lambda f, region, work: iter([f]))
    monkeypatch.setattr(overlay, "_adaptive_crops", lambda f, region, work: iter([f]))


@pytest.mark.parametrize("workers", [1, 2])
def test_a_missing_engine_skips_the_recognizer_instead_of_failing_the_run(
    monkeypatch, tmp_path, workers
):
    """The contract the adapter-level tests imply but do not reach: with no OCR
    stack installed, the pipeline records a skip and the run continues.

    This is what makes the engine's import site safe to move. The adapter now
    raises on first read rather than at construction, so the raise happens
    inside the reader — and, at workers>1, inside a pool. Both are covered
    here: `Future.result()` re-raises in the calling thread, so the exception
    type survives, and the pipeline wraps construction and recognition in one
    try either way.
    """
    import sys

    from dirstral_annotator.pipeline import Pipeline
    from dirstral_annotator.roster import Roster

    monkeypatch.setitem(sys.modules, "pytesseract", None)
    _one_scripted_frame(monkeypatch, tmp_path)
    monkeypatch.setenv("DIRSTRAL_ANNOTATOR_WORKERS", str(workers))

    media = tmp_path / "clip.mp4"
    media.write_bytes(b"stand-in; frame extraction is patched out")

    pipeline = Pipeline(roster=Roster([]), scorebug=True)
    cues = pipeline.cues_for(media)

    assert cues == []
    assert pipeline.skipped, "a missing OCR stack produced no skip note"
    note = " ".join(pipeline.skipped).lower()
    assert "ocr" in note or "pytesseract" in note or "pillow" in note, \
        f"the skip note does not say what is missing: {pipeline.skipped}"
