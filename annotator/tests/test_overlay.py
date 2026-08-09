"""The generic overlay-text reader: what it must do for any corpus.

Everything here is language- and sport-free on purpose. The reader's whole
claim is that finding burned-in text and getting a clean string out of it is
one capability, separable from what the string means, so these tests pin the
capability and never the interpretation. The pilot corpus is an English
baseball broadcast and the second is Russian news; neither may show up here as
an assumption.

Backend-free except where a test is explicitly about Pillow or pytesseract, in
which case the module is faked.
"""

from __future__ import annotations

import ast
import sys
import threading
from concurrent.futures import Future
from pathlib import Path

import pytest

from dirstral_annotator.recognizers import overlay
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.overlay import (
    OverlayRead,
    OverlayReader,
    _RegionSearch,
    any_text,
    map_in_order,
)

MEDIA = "broadcast.mp4"
WHOLE = (0.0, 0.0, 1.0, 1.0)
# Two bands of a hypothetical frame, named for what a caller would find there.
BACKGROUND = (0.00, 0.00, 1.00, 0.20)
BADGE = (0.00, 0.80, 1.00, 0.20)


# --- region search ---------------------------------------------------------

def test_search_sweeps_on_a_stride_then_locks():
    regions = (("a",), ("b",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    assert search.regions_for(0) == regions
    assert search.regions_for(1) == ()
    for _ in range(overlay.LOCK_AFTER_READS):
        search.record(regions[1], hits=1)
    assert search.regions_for(0) == (regions[1],)


def test_a_single_region_is_locked_from_the_start():
    search = _RegionSearch((("only",),))  # type: ignore[arg-type]
    assert search.regions_for(1) == (("only",),)


def test_lock_releases_when_the_overlay_moves():
    regions = (("a",), ("b",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    for _ in range(overlay.LOCK_AFTER_READS):
        search.record(regions[0], hits=1)
    for _ in range(_RegionSearch.RELEASE_AFTER_MISSES):
        search.record(regions[0], hits=0)
    assert search.regions_for(0) == regions


# --- the reader yields text and nothing else -------------------------------

@pytest.fixture
def bands(monkeypatch, tmp_path):
    """Drive the reader off a per-band script, without images or an engine.

    `install({region: text, ...}, frames=n)` makes every sampled frame answer
    with that text for that band, which is the shape a real overlay has: one
    band holds the graphic, the others hold whatever was behind it.
    """

    def install(script, frames=24, fps=0.5):
        listing = []
        for i in range(frames):
            path = tmp_path / f"frame-{i:05d}.jpg"
            path.write_text(str(i))
            listing.append((i / fps, path))
        monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(listing))
        monkeypatch.setattr(
            overlay,
            "read_band",
            lambda ocr, frame, region, work: [script.get(tuple(region), "")],
        )
        return listing

    return install


def test_a_read_carries_the_text_its_time_and_its_place(bands):
    bands({WHOLE: "SEGMENT TITLE"}, frames=3)
    reads = list(OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=1).read_text(MEDIA))
    assert [r.timestamp_s for r in reads] == [0.0, 2.0, 4.0]
    assert {r.region for r in reads} == {WHOLE}
    assert all(r.texts == ("SEGMENT TITLE",) for r in reads)
    assert reads[0].index == 0


def test_the_reader_keeps_every_preprocessing_pass(monkeypatch, tmp_path):
    """Both passes reach the caller, in order and undeduplicated: which one is
    right depends on the frame, and only the caller can decide."""
    frame = tmp_path / "frame-00000.jpg"
    frame.write_text("x")
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter([(0.0, frame)]))
    monkeypatch.setattr(
        overlay, "_prepared_crops", lambda frame, region, work: iter([frame, frame])
    )
    passes = iter(["grey read", "binarised read"])
    reader = OverlayReader(ocr=lambda p: next(passes), crop=WHOLE, workers=1)
    assert next(iter(reader.read_text(MEDIA))).texts == ("grey read", "binarised read")


def test_best_is_the_longest_pass():
    assert OverlayRead(0, 0.0, WHOLE, ("21:3", "21:35 MCK")).best() == "21:35 MCK"
    assert OverlayRead(0, 0.0, WHOLE, ()).best() == ""


# --- the band lock runs on the caller's judgement --------------------------

def test_the_default_interpreter_cannot_tell_an_overlay_from_a_shop_sign(bands):
    """`any_text` locks onto the first band that returned anything, which for a
    frame with text in the background is the wrong band. This is not a bug in
    the default; it is why the hit count is a parameter."""
    bands({BACKGROUND: "lorem ipsum dolor", BADGE: "21:35 MCK"})
    reader = OverlayReader(
        ocr=lambda p: "", regions=(BACKGROUND, BADGE), workers=1
    )
    assert {r.region for r in reader.read_text(MEDIA)} == {BACKGROUND}


def test_a_caller_that_can_tell_them_apart_moves_the_lock(bands):
    """The same footage, the same reader, one different callback: the search
    now lands on the band that holds the graphic the caller came for.

    This is the whole seam. A reader that scored bands itself would have to
    guess what "useful" means, and on real footage guessing "not empty" is how
    three hours of OCR ends up pointed at the crowd.
    """
    bands({BACKGROUND: "lorem ipsum dolor", BADGE: "21:35 MCK"})
    reader = OverlayReader(ocr=lambda p: "", regions=(BACKGROUND, BADGE), workers=1)

    def looks_like_a_clock(read):
        text = read.best()
        return text, sum(":" in part for part in text.split())

    reads = [read for read, _ in reader.read(MEDIA, looks_like_a_clock)]
    # Once locked, the reader pays for one crop, and it is the caller's.
    assert {read.region for read in reads[-8:]} == {BADGE}


def test_any_text_ignores_debris_but_keeps_a_word():
    assert any_text(OverlayRead(0, 0.0, WHOLE, ("", " a ", "\n"))) == ((), 0)
    kept, hits = any_text(OverlayRead(0, 0.0, WHOLE, ("21:35 MCK", "")))
    assert kept == ("21:35 MCK",) and hits == 1


# --- parallelism is invisible ----------------------------------------------

def test_parallel_reading_matches_serial(bands):
    bands({BACKGROUND: "", BADGE: "21:35 MCK"}, frames=60)
    serial = list(OverlayReader(ocr=lambda p: "", regions=(BACKGROUND, BADGE),
                                workers=1).read_text(MEDIA))
    bands({BACKGROUND: "", BADGE: "21:35 MCK"}, frames=60)
    parallel = list(OverlayReader(ocr=lambda p: "", regions=(BACKGROUND, BADGE),
                                  workers=4).read_text(MEDIA))
    assert serial == parallel and serial


def test_a_worker_that_cannot_read_surfaces_as_unavailable(monkeypatch, tmp_path):
    """A missing backend has to degrade the cascade, so it must arrive at the
    caller as RecognizerUnavailable rather than as a broken pool."""
    frame = tmp_path / "frame-00000.jpg"
    frame.write_text("x")
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter([(0.0, frame)] * 8))
    monkeypatch.setattr(
        overlay, "_prepared_crops", lambda frame, region, work: iter([frame])
    )

    def refuses(path):
        raise RecognizerUnavailable("Pillow is required for overlay OCR")

    with pytest.raises(RecognizerUnavailable):
        list(OverlayReader(ocr=refuses, crop=WHOLE, workers=4).read_text(MEDIA))


def test_reads_leave_the_calling_thread_under_the_readers_own_name(bands, monkeypatch):
    bands({WHOLE: "text"}, frames=40)
    seen: set[str] = set()
    lock = threading.Lock()
    real = overlay.read_band

    def watched(ocr, frame, region, work):
        with lock:
            seen.add(threading.current_thread().name)
        return real(ocr, frame, region, work)

    monkeypatch.setattr(overlay, "read_band", watched)
    reader = OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=4, name="ticker")
    list(reader.read_text(MEDIA))
    assert len(seen) > 1, f"reads never left the calling thread: {seen}"
    assert all(name.startswith("ticker") for name in seen), seen


class _ReverseExecutor:
    """Runs everything, but only hands back results in reverse submit order.

    The worst case for any implementation that lets completion order reach the
    caller: nothing runs until a result is awaited, and then the most recently
    submitted task runs first.
    """

    def __init__(self):
        self._queued: list[tuple[Future, object, tuple]] = []

    def submit(self, fn, *args):
        future: Future = Future()
        self._queued.append((future, fn, args))
        return future

    def drain(self):
        while self._queued:
            future, fn, args = self._queued.pop()  # newest first
            if future.set_running_or_notify_cancel():
                future.set_result(fn(*args))


def test_map_in_order_yields_input_order_whatever_finishes_first(monkeypatch):
    pool = _ReverseExecutor()
    original = Future.result

    def result(self, timeout=None):
        pool.drain()
        return original(self, timeout)

    monkeypatch.setattr(Future, "result", result)
    items = [(f"key-{i}", i) for i in range(20)]
    got = list(map_in_order(pool, lambda n: n * 2, items, lookahead=4))
    assert got == [(f"key-{i}", i * 2) for i in range(20)]


def test_the_lookahead_scales_with_the_pool_and_has_a_floor():
    assert overlay.lookahead_for(1) == overlay.MIN_LOOKAHEAD
    assert overlay.lookahead_for(8) == 8 * overlay.LOOKAHEAD_PER_WORKER


# --- no language and no corpus baked in ------------------------------------

def test_the_reader_imports_nothing_corpus_specific():
    """Structural, because a docstring cannot be linted. The reader may know
    about frames and OCR; it may not know about rosters, players or any one
    recognizer, or the seam has leaked."""
    tree = ast.parse(Path(overlay.__file__).read_text(encoding="utf-8"))
    imported: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module:
            imported.add(node.module.split(".")[-1])
        elif isinstance(node, ast.Import):
            imported.update(alias.name.split(".")[0] for alias in node.names)
    forbidden = {"scorebug", "jersey", "faces", "playbyplay", "roster", "model",
                 "fusion"}
    assert not (imported & forbidden), imported & forbidden


class _FakeTesseract:
    """Records what the adapter asked of the engine."""

    def __init__(self):
        self.calls: list[dict] = []

    def image_to_string(self, image, config="", lang=None):
        self.calls.append({"config": config, "lang": lang})
        return "text"


@pytest.fixture
def fake_tesseract(monkeypatch):
    fake = _FakeTesseract()
    monkeypatch.setitem(sys.modules, "pytesseract", fake)
    return fake


@pytest.fixture
def jpeg(tmp_path):
    pillow = pytest.importorskip("PIL.Image")
    path = tmp_path / "frame-00000001.jpg"
    pillow.new("RGB", (640, 360), (20, 20, 20)).save(path)
    return path


def test_the_ocr_language_is_a_parameter(monkeypatch, fake_tesseract, jpeg):
    monkeypatch.delenv(overlay.LANG_ENV, raising=False)
    overlay.default_ocr()(jpeg)
    overlay.default_ocr(lang="rus")(jpeg)
    overlay.default_ocr(lang="eng+rus")(jpeg)
    assert [call["lang"] for call in fake_tesseract.calls] == [None, "rus", "eng+rus"]


def test_the_ocr_language_can_come_from_the_environment(
    monkeypatch, fake_tesseract, jpeg
):
    """So an operator can point the cascade at a corpus in another script
    without a code change. An explicit argument still wins."""
    monkeypatch.setenv(overlay.LANG_ENV, "rus")
    overlay.default_ocr()(jpeg)
    overlay.default_ocr(lang="eng")(jpeg)
    assert [call["lang"] for call in fake_tesseract.calls] == ["rus", "eng"]
    monkeypatch.setenv(overlay.LANG_ENV, "   ")
    assert overlay.default_lang() is None


def test_the_page_segmentation_mode_is_a_parameter(
    monkeypatch, fake_tesseract, jpeg
):
    monkeypatch.delenv(overlay.LANG_ENV, raising=False)
    overlay.default_ocr()(jpeg)
    overlay.default_ocr(psm=overlay.OCR_PSM)(jpeg)
    assert [call["config"] for call in fake_tesseract.calls] == ["", "--psm 6"]


def test_a_reader_reports_the_language_it_settled_on(monkeypatch):
    """Resolved, not echoed: a caller inspecting the reader should see what it
    will actually OCR in, whether that came from the argument or the
    environment."""
    monkeypatch.setenv(overlay.LANG_ENV, "rus")
    assert OverlayReader(ocr=lambda p: "").lang == "rus"
    assert OverlayReader(ocr=lambda p: "", lang="eng").lang == "eng"
    monkeypatch.delenv(overlay.LANG_ENV)
    assert OverlayReader(ocr=lambda p: "").lang is None


def test_a_missing_engine_degrades_instead_of_aborting(monkeypatch, tmp_path):
    """A missing engine must surface as RecognizerUnavailable, which the
    pipeline catches and records as a skip, rather than an unhandled
    ImportError that takes the run down.

    The raise happens on the first READ, not when the adapter is built:
    building is not using it, and importing eagerly made constructing any
    overlay recognizer fail without the `ocr` extra even when the caller
    supplied its own reader and never OCR'd a frame.
    """
    monkeypatch.setitem(sys.modules, "pytesseract", None)
    ocr = overlay.default_ocr()  # building the adapter needs no engine
    with pytest.raises(RecognizerUnavailable):
        ocr(tmp_path / "frame.jpg")


# --- preprocessing ---------------------------------------------------------

def test_both_passes_are_rendered_and_the_second_is_binary(jpeg, tmp_path):
    """Light-on-dark text defeats Otsu, so a hard threshold runs as well as the
    grey pass; on the pilot fixture 44 of 215 readable frames were readable
    only after it."""
    image = pytest.importorskip("PIL.Image")
    crops = list(overlay._prepared_crops(jpeg, (0.0, 0.8, 1.0, 0.2), tmp_path))
    assert len(crops) == 2 and all(p.exists() for p in crops)
    with image.open(crops[0]) as grey:
        # 360 * 0.2 = 72 rows, upscaled towards TARGET_BAND_PX and capped.
        assert grey.height == 72 * int(overlay.MAX_UPSCALE)
    with image.open(crops[1]) as binary:
        # Nothing between the two extremes survived the threshold.
        assert sum(binary.histogram()[1:255]) == 0


def test_a_degenerate_region_renders_nothing(jpeg, tmp_path):
    assert list(overlay._prepared_crops(jpeg, (0.0, 0.0, 0.0, 0.0), tmp_path)) == []


def test_missing_pillow_degrades_the_cascade(monkeypatch, tmp_path):
    """_prepared_crops must raise RecognizerUnavailable, not ImportError, so the
    pipeline skips the recognizer instead of aborting the whole run."""
    import builtins

    real_import = builtins.__import__

    def no_pil(name, *a, **kw):
        if name == "PIL" or name.startswith("PIL."):
            raise ImportError("No module named 'PIL'")
        return real_import(name, *a, **kw)

    monkeypatch.setattr(builtins, "__import__", no_pil)
    frame = tmp_path / "frame-00000001.jpg"
    frame.write_bytes(b"\xff\xd8\xff")
    with pytest.raises(RecognizerUnavailable):
        list(overlay._prepared_crops(frame, WHOLE, tmp_path))



def _write_solid_png(path):
    """A real, decodable one-pixel image, so the test reaches the OCR call
    rather than failing in Pillow on the way there."""
    from PIL import Image

    Image.new("RGB", (4, 4), (0, 0, 0)).save(path)

def test_an_installed_package_with_no_engine_also_degrades(monkeypatch, tmp_path):
    """pytesseract installs from PyPI; tesseract is a separate native binary.
    "Package present, engine absent" is an ordinary deployment state -- a CI
    runner with the `ocr` extra and no apt package is exactly it -- and
    TesseractNotFoundError is not RecognizerUnavailable, so without translation
    the cascade aborts instead of skipping.
    """
    pytesseract = pytest.importorskip("pytesseract")
    pytest.importorskip("PIL.Image")

    def missing_engine(*args, **kwargs):
        raise pytesseract.TesseractNotFoundError()

    monkeypatch.setattr(pytesseract, "image_to_string", missing_engine)

    frame = tmp_path / "band.png"
    _write_solid_png(frame)

    with pytest.raises(RecognizerUnavailable) as caught:
        overlay.default_ocr()(frame)
    assert "tesseract" in str(caught.value).lower()
