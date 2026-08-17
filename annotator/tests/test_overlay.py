"""The generic overlay-text reader: what it must do for any corpus.

Everything here is language- and sport-free on purpose. The reader's whole
claim is that finding burned-in text and getting a clean string out of it is
one capability, separable from what the string means, so these tests pin the
capability and never the interpretation. The pilot corpus is an English
baseball broadcast and the second is Russian news; neither may show up here as
an assumption.

Backend-free except where a test is explicitly about Pillow or pytesseract, in
which case the module is faked. The fallback rendering is the one exception that
needs the real thing: what it claims is that certain pixels come back as text
that the shipped passes lose, and no fake can hold that claim up. Those tests
generate their frames with Pillow and read them with the installed tesseract,
and skip where either is missing.
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

    def install(script, frames=24, fps=0.5, fallback=None, seen=None):
        listing = []
        for i in range(frames):
            path = tmp_path / f"frame-{i:05d}.jpg"
            path.write_text(str(i))
            listing.append((i / fps, path))
        monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(listing))

        def read(ocr, frame, region, work):
            return [script.get(tuple(region), "")]

        def read_adaptive(ocr, frame, region, work):
            if seen is not None:
                seen.append((frame.name, tuple(region)))
            # `fallback` is what the SECOND rendering of this band would say. By
            # default it says what the first one did: a script states what is on
            # the frame, and no rendering puts a graphic there that was not.
            source = script if fallback is None else fallback
            return [source.get(tuple(region), "")]

        monkeypatch.setattr(overlay, "read_band", read)
        monkeypatch.setattr(overlay, "read_band_adaptive", read_adaptive)
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
    monkeypatch.setattr(
        overlay, "_adaptive_crops", lambda frame, region, work: iter([frame])
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


# --- the adaptive fallback ---------------------------------------------------

def test_a_band_that_read_something_is_never_re_read(bands):
    """The cost contract, counted rather than described. A run whose shipped
    passes already answer must not pay for the fallback at all, which is what
    keeps this rendering off corpora that do not need it."""
    retried: list[tuple[str, tuple]] = []
    bands({WHOLE: "SEGMENT TITLE"}, frames=12, seen=retried)
    list(OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=1).read_text(MEDIA))
    assert retried == []


def test_a_band_that_read_nothing_is_re_read_once_within_a_budget(bands):
    """A miss is ordinary, not exceptional: every background band misses. So a
    run gets a fixed number of attempts to show the rendering helps, and stops
    paying once it has not."""
    retried: list[tuple[str, tuple]] = []
    bands({WHOLE: ""}, frames=80, seen=retried)
    list(OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=1).read_text(MEDIA))
    assert len(retried) == overlay.FALLBACK_TRIALS
    # One re-read per missed band, never two.
    assert len(set(retried)) == len(retried)


def test_a_recovered_read_replaces_the_one_that_missed(bands):
    """What reaches the caller is the rendering that found the text. The read
    that came back empty is not also yielded: one band is one read."""
    bands({WHOLE: ""}, frames=4, fallback={WHOLE: "FAINT HEADLINE"})
    reads = list(OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=1).read_text(MEDIA))
    assert [r.texts for r in reads] == [("FAINT HEADLINE",)] * 4


def test_a_fallback_read_keeps_the_run_going_after_the_budget(bands):
    """One hit arms the fallback for the rest of the file. A corpus that needs
    the rendering needs it throughout, so a budget that expired mid-file would
    stop reading the overlay halfway down a broadcast."""
    frames = overlay.FALLBACK_TRIALS * 3
    bands({WHOLE: ""}, frames=frames, fallback={WHOLE: "FAINT HEADLINE"})
    reads = list(OverlayReader(ocr=lambda p: "", crop=WHOLE, workers=1).read_text(MEDIA))
    assert [r.texts for r in reads] == [("FAINT HEADLINE",)] * frames


def test_a_noisy_fallback_read_does_not_take_the_band_from_a_clean_one(bands):
    """The trade this rendering makes. It recovers text the shipped passes lose
    AND it emits junk tokens from ordinary background texture, so on a caller
    that counts any text as evidence it produces hits on the wrong band. The
    band lock is where that would do real damage: it decides what gets OCR'd for
    the rest of the file.

    So a fallback hit does not end the frame and cannot outrank a clean read.
    Here the background band answers only under the fallback and the badge
    answers on the shipped passes, and the badge has to win.
    """
    bands(
        {BACKGROUND: "", BADGE: "21:35 MCK"},
        frames=40,
        fallback={BACKGROUND: "lorem ipsum dolor", BADGE: ""},
    )
    reader = OverlayReader(ocr=lambda p: "", regions=(BACKGROUND, BADGE), workers=1)
    reads = list(reader.read_text(MEDIA))
    assert {read.region for read in reads[-8:]} == {BADGE}


def test_a_fallback_hit_cannot_lock_a_band_against_a_clean_read():
    """The same rule at the level it is enforced. One clean read anywhere is
    enough: the fallback's own score never reaches the lock after it."""
    regions = (("noisy",), ("clean",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    search.record(regions[1], hits=1)
    for _ in range(overlay.LOCK_AFTER_READS * 3):
        search.record(regions[0], hits=1, fallback=True)
    assert search.locked is None
    for _ in range(overlay.LOCK_AFTER_READS - 1):
        search.record(regions[1], hits=1)
    assert search.regions_for(0) == (regions[1],)


def test_a_fallback_hit_locks_a_band_when_nothing_else_read_anything():
    """The lock is not withheld out of principle. On footage only the fallback
    can read, it is the evidence there is, and without the lock the reader would
    sweep every band for three hours and only ever read on the search stride."""
    regions = (("a",), ("b",))
    search = _RegionSearch(regions)  # type: ignore[arg-type]
    for _ in range(overlay.LOCK_AFTER_READS):
        search.record(regions[1], hits=1, fallback=True)
    assert search.regions_for(1) == (regions[1],)


def test_giving_up_on_the_fallback_is_per_run():
    """`read()` builds its own, so a file the rendering cannot help does not
    disarm it for the next file a recognizer is pointed at."""
    spent = overlay._AdaptiveFallback(trials=1)
    spent.record(0)
    assert not spent.wanted()
    assert overlay._AdaptiveFallback().wanted()


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
    monkeypatch.setattr(
        overlay, "_adaptive_crops", lambda frame, region, work: iter([frame])
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
    assert list(overlay._adaptive_crops(jpeg, (0.0, 0.0, 0.0, 0.0), tmp_path)) == []


def test_the_fallback_renders_one_local_mean_per_radius(jpeg, tmp_path):
    """Same band, same upscale, one bitonal image per radius. Two of them
    because an interpreter's evidence can be agreement between passes, and one
    rendering gives it nothing to compare against."""
    image = pytest.importorskip("PIL.Image")
    crops = list(overlay._adaptive_crops(jpeg, (0.0, 0.8, 1.0, 0.2), tmp_path))
    assert len(crops) == len(overlay.ADAPTIVE_RADII)
    assert len(set(crops)) == len(crops) and all(p.exists() for p in crops)
    for crop in crops:
        with image.open(crop) as rendered:
            assert rendered.height == 72 * int(overlay.MAX_UPSCALE)
            # Bitonal: a local mean is still a threshold.
            assert sum(rendered.histogram()[1:255]) == 0


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
    with pytest.raises(RecognizerUnavailable):
        list(overlay._adaptive_crops(frame, WHOLE, tmp_path))



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


# --- what the fallback rendering recovers, read for real ---------------------
#
# The claim under test is about pixels, so these read synthetic frames with the
# installed tesseract. They skip where the engine, Pillow or a scalable font is
# missing, which is the state of a plain CI runner.
#
# The frames bracket a broadcast rather than copy one: 1920x1080, JPEG q55, one
# overlay composited at a stated opacity over coarse bright and dark patches.
# Latin text and `eng`, because the reader takes its language as a parameter and
# a test may not assume a corpus; the failure has nothing to do with script.

HEADLINE = "BREAKING NEWS TODAY"
PANEL = (0.0, 0.80, 1.0, 0.10)  # where the overlay is drawn, in fractions
SCALABLE_FONTS = (
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
    "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
)


def _headline_font(size):
    from PIL import ImageFont

    for candidate in SCALABLE_FONTS:
        if Path(candidate).exists():
            return ImageFont.truetype(candidate, size)
    pytest.skip("no scalable font on this host; the bundled bitmap face cannot "
                "render broadcast-sized text")


def _scene(size):
    """A frame's worth of bright and dark, at a scale coarser than a stroke.

    This is what makes a semi-transparent panel hard: the scene shows through,
    so the same ink is darker over a dark patch than over a bright one and no
    single global cut holds across the band.
    """
    from PIL import Image, ImageDraw, ImageFilter

    scene = Image.new("RGB", size)
    draw = ImageDraw.Draw(scene)
    pitch = 240
    for x in range(0, size[0], pitch):
        for y in range(0, size[1], pitch):
            shade = (20, 235, 90, 200)[(x // pitch + y // pitch) % 4]
            draw.rectangle(
                [x, y, x + pitch - 1, y + pitch - 1],
                fill=(shade, max(0, shade - 30), min(255, shade + 20)),
            )
    return scene.filter(ImageFilter.GaussianBlur(6))


def _overlay_frame(path, *, opacity, dark_on_light, size_px=52, scene=True):
    """One frame carrying one overlay at `opacity` out of 255."""
    from PIL import Image, ImageDraw

    size = (1920, 1080)
    base = _scene(size) if scene else Image.new("RGB", size, (128, 128, 128))
    graphic = base.copy()
    draw = ImageDraw.Draw(graphic)
    top = int(size[1] * PANEL[1])
    bottom = int(size[1] * (PANEL[1] + PANEL[3]))
    panel = (245, 245, 245) if dark_on_light else (12, 12, 12)
    draw.rectangle([0, top, size[0], bottom], fill=panel)
    draw.text(
        (60, top + 10),
        HEADLINE,
        font=_headline_font(size_px),
        fill=(18, 18, 18) if dark_on_light else (250, 250, 250),
    )
    Image.blend(base, graphic, opacity / 255).save(path, "JPEG", quality=55)
    return path


def _read_it(text):
    """Whether the headline came out of this pass.

    Every word, matched inside the alphanumeric run rather than token by token,
    so the graphic's own edges arriving welded to a word ("IBREAKING") still
    counts as having read it. The failures this separates are not near misses:
    they return either nothing or an unrelated string.
    """
    body = "".join(ch for ch in text.upper() if ch.isalnum())
    return all(word in body for word in HEADLINE.split())


@pytest.fixture
def engine(tmp_path):
    """The installed tesseract, or a skip, plus a control read.

    The control is an opaque panel on a flat field: if the engine cannot read
    THAT, the host's OCR stack is the problem and the measurements below would
    report a fixture failure as a code failure.
    """
    pytest.importorskip("pytesseract")
    pytest.importorskip("PIL.Image")
    ocr = overlay.default_ocr(psm=overlay.OCR_PSM, lang="eng")
    control = _overlay_frame(
        tmp_path / "control.jpg", opacity=255, dark_on_light=True, scene=False
    )
    try:
        passes = overlay.read_band(ocr, control, PANEL, tmp_path)
    except RecognizerUnavailable as unavailable:
        pytest.skip(f"no usable OCR engine: {unavailable}")
    if not any(_read_it(text) for text in passes):
        pytest.skip(f"this OCR stack cannot read the control frame: {passes}")
    return ocr


@pytest.mark.parametrize("opacity", [255, 170])
def test_a_clean_dark_on_light_panel_reads_on_the_shipped_passes(
    engine, tmp_path, opacity
):
    """The premise this work started from, and it does not hold: the two shipped
    passes are not light-on-dark only. `BINARY_THRESHOLD` keeps p > 140, so dark
    ink on a light panel arrives as black text on white already. There is
    nothing to invert, and this pins that no fallback is needed here."""
    frame = _overlay_frame(
        tmp_path / f"clean-{opacity}.jpg", opacity=opacity, dark_on_light=True
    )
    passes = overlay.read_band(engine, frame, PANEL, tmp_path)
    assert any(_read_it(text) for text in passes), passes


@pytest.mark.parametrize("opacity", [255, 170, 110])
def test_light_on_dark_reads_at_every_opacity_on_the_shipped_passes(
    engine, tmp_path, opacity
):
    """Bright ink stays above a global cut whatever shows through the panel, so
    this polarity never needed the fallback and must not start needing it."""
    frame = _overlay_frame(
        tmp_path / f"bright-{opacity}.jpg", opacity=opacity, dark_on_light=False
    )
    passes = overlay.read_band(engine, frame, PANEL, tmp_path)
    assert any(_read_it(text) for text in passes), passes


def test_a_faint_dark_on_light_panel_is_read_only_by_the_fallback(engine, tmp_path):
    """The failure the fallback exists for, and the whole measurement in one
    test: at opacity 110 both shipped passes lose a headline a human reads
    easily, and the local-mean renderings return it.

    Both halves are asserted. If the shipped passes ever start reading this
    frame the fallback has stopped being justified by it, and that is a result
    to re-measure rather than a test to loosen.
    """
    frame = _overlay_frame(tmp_path / "faint.jpg", opacity=110, dark_on_light=True)
    shipped = overlay.read_band(engine, frame, PANEL, tmp_path)
    assert not any(_read_it(text) for text in shipped), shipped
    recovered = overlay.read_band_adaptive(engine, frame, PANEL, tmp_path)
    assert all(_read_it(text) for text in recovered), recovered


def test_the_reader_recovers_the_faint_panel_end_to_end(engine, monkeypatch, tmp_path):
    """The wiring, on real pixels: the band the shipped passes missed reaches
    the caller with its text, because the reader re-read it once the caller's
    interpretation came back with no hits."""
    frame = _overlay_frame(tmp_path / "faint.jpg", opacity=110, dark_on_light=True)
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter([(0.0, frame)]))
    reader = OverlayReader(ocr=engine, crop=PANEL, workers=1)
    (read,) = list(reader.read_text(MEDIA))
    assert _read_it(read.best()), read.texts


class _NoFallback:
    """The reader as it was before the fallback existed."""

    def wanted(self) -> bool:
        return False

    def record(self, hits: int) -> None:
        pass


def test_the_same_frame_is_unread_with_the_fallback_switched_off(
    engine, monkeypatch, tmp_path
):
    """The other half of the test above, and what makes it worth having: with
    the re-read disabled the reader comes back with no headline at all. Without
    this, a frame the shipped passes could read all along would pass the test
    above and prove nothing."""
    frame = _overlay_frame(tmp_path / "faint.jpg", opacity=110, dark_on_light=True)
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter([(0.0, frame)]))
    monkeypatch.setattr(overlay, "_AdaptiveFallback", _NoFallback)
    reader = OverlayReader(ocr=engine, crop=PANEL, workers=1)
    (read,) = list(reader.read_text(MEDIA))
    assert not _read_it(read.best()), read.texts
