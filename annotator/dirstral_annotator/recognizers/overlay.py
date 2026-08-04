"""Overlay-text reader: pull burned-in text off sampled frames, time-anchored.

Most archive footage carries no structured feed, so the only machine-readable
description of what is on screen is what the broadcaster burned into the
picture: a sports scorebug, a news headline banner, a scrolling ticker, a
segment title, a clock badge. Reading those reliably is one capability, and it
is the hard half. What the strings *mean* is not: a sports bug names players
against a roster, a news banner names a topic, and no single module can know
which. This module does the reading and stops there.

Reading well takes three things, and the naive "OCR the whole frame" loop gets
none of them:

1. *Where.* An overlay occupies a few percent of the frame. Whole-frame OCR has
   to segment everything else before it reaches the 30-pixel-tall text, and
   usually never does. A handful of candidate bands are OCR'd instead, and
   whichever one actually produces reads is locked onto so the rest of the run
   pays for one crop. See `_RegionSearch`.
2. *How.* Overlay text is light-on-dark, small and JPEG-soft. Upscaling the
   crop to a fixed text height and binarising it turns a garbled read into a
   clean one. The two passes (grey and hard-thresholded) disagree often enough
   on real footage to be worth running both: on the pilot fixture 44 of 215
   readable frames were readable ONLY after thresholding, because a fixed cut
   handles light-on-dark that Otsu does not. See `_prepared_crops`.
3. *Fast enough.* Reading a frame costs about a second, so a three hour
   broadcast is nearly two hours of OCR. It is embarrassingly parallel, so it
   runs on a worker pool, and the pool is arranged so that completion order can
   never reach the caller's output. See `_PooledReader` and `_with_lookahead`.

Nothing here assumes a language, a script or a corpus. The OCR engine is
injected as a callable `(image_path) -> str`; the default adapter uses
pytesseract when installed and takes its language as a parameter (or from
`DIRSTRAL_ANNOTATOR_OCR_LANG`), never a hardcoded one. Callers that know their
corpus pass their own regions and their own interpretation.

The interpretation seam is `OverlayReader.read(media, interpret)`. `interpret`
receives one band's text and returns whatever it made of it plus a *hit count*:
how much evidence this band held. That count is the only thing the reader takes
back, and it is what drives the band lock. It has to come from the caller
because "this band is the overlay" is a judgement about content: a crop of
crowd texture yields plenty of text, and locking onto it because it was not
empty is precisely how a reader ends up OCR'ing the wrong 3% of every frame for
three hours. The default interpreter (`any_text`) is deliberately weak and is
for callers with nothing better to offer.
"""

from __future__ import annotations

import os
import tempfile
import threading
from collections import deque
from collections.abc import Callable, Iterable, Iterator
from concurrent.futures import Executor, Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Self, TypeVar

from .base import RecognizerUnavailable, iter_frames

OcrFn = Callable[[Path], str]
#: Fractional (x, y, w, h) box, so one set of bands fits SD, HD and 4K alike.
Region = tuple[float, float, float, float]

# Fractional boxes searched for the overlay, covering the corners broadcasts
# actually use: sports bugs hang top left or along the bottom. Deliberately
# generous, because a band that clips the text is worse than one that includes
# some background.
CANDIDATE_REGIONS: tuple[Region, ...] = (
    (0.00, 0.02, 0.55, 0.16),  # upper left
    (0.00, 0.80, 0.55, 0.18),  # lower left
    (0.20, 0.80, 0.60, 0.18),  # lower centre
    (0.20, 0.02, 0.60, 0.16),  # upper centre
)

# Full-width bands, for overlays that span the frame: news tickers, lower
# thirds, headline banners. Not in the default sweep, because a wider crop is a
# slower OCR and a noisier one; a caller that knows its corpus asks for these.
WIDE_REGIONS: tuple[Region, ...] = (
    (0.00, 0.78, 1.00, 0.22),  # lower band: ticker, lower third
    (0.00, 0.00, 1.00, 0.20),  # upper band: headline banner, clock badge
)

# Crops are upscaled so the band is about this tall before OCR. Fixed pixels,
# not a fixed factor, so an SD archive gets the magnification it needs and a
# 4K master is not blown up into a slow no-op.
TARGET_BAND_PX = 320
MAX_UPSCALE = 4.0
# Everything darker than this becomes black in the second pass. Overlay text is
# near-white by design, so the cut can sit high.
BINARY_THRESHOLD = 140
# tesseract page-segmentation mode for a cropped overlay strip: "a single
# uniform block of text". Tesseract's default keeps hunting for page structure
# that a 200-pixel-tall strip does not have.
OCR_PSM = 6

# Region search: while unlocked, only every Nth frame pays for the full sweep;
# once one band has produced this many reads it is the only one OCR'd.
SCAN_STRIDE = 4
LOCK_AFTER_READS = 5

# Worker pool for the per-frame read. The default is a quarter of the core
# count, not all of it, for a measured reason: tesseract is built against
# OpenMP and runs four threads per page (NLWP 4 on every invocation), so N
# workers put 4N threads on the machine. On the pilot's 16 core host that
# makes 4 workers the fastest setting (3.0x to 3.3x on the scorebug), 8
# workers slower than running serially (0.83x to 0.90x) and 12 workers four
# times slower than serial. Raise DIRSTRAL_ANNOTATOR_WORKERS where tesseract
# is built without OpenMP, or where the host has cores to spare. Setting
# OMP_THREAD_LIMIT=1 makes each worker use one core and lets the count go
# higher, but it is process wide: it would also throttle the torch and
# onnxruntime work the jersey and face recognizers do in the same process.
WORKERS_ENV = "DIRSTRAL_ANNOTATOR_WORKERS"
OCR_THREADS_PER_WORKER = 4
MAX_DEFAULT_WORKERS = 8
# How far ahead of the authoritative loop frames are speculatively dispatched.
# Has to exceed the worker count or the pool drains while the main thread folds
# one frame's reads into its own state; a few frames per worker is enough, and
# each pending entry is only a path and a short result list.
LOOKAHEAD_PER_WORKER = 4
MIN_LOOKAHEAD = 16

#: Language(s) handed to the default OCR adapter, in tesseract's own notation
#: ("rus", "eng+rus"). Unset means tesseract's built-in default. This exists so
#: an operator can point the whole cascade at a corpus in another script
#: without a code change; a caller that knows better passes `lang=` instead.
LANG_ENV = "DIRSTRAL_ANNOTATOR_OCR_LANG"

#: Shortest stripped text `any_text` will treat as evidence of an overlay.
#: Below this a read is as likely to be JPEG noise as a word.
MIN_USEFUL_CHARS = 4


def default_lang() -> str | None:
    """Configured OCR language, or None for the engine's own default."""
    return os.environ.get(LANG_ENV, "").strip() or None


def default_ocr(psm: int | None = None, lang: str | None = None) -> OcrFn:
    """pytesseract adapter, optionally pinned to a page-segmentation mode.

    `psm` is left alone by default because other recognizers share this adapter
    on crops of their own shape; the overlay reader asks for `OCR_PSM`.

    `lang` is a parameter and never a constant. The pilot corpus is English and
    the second is Russian, and neither is a property of this code: passing None
    falls back to `DIRSTRAL_ANNOTATOR_OCR_LANG` and then to whatever the local
    tesseract defaults to. Non-Latin scripts need the matching traineddata
    installed (`tesseract-lang` on Homebrew, `tesseract-ocr-rus` on Debian).
    """
    try:
        import pytesseract  # type: ignore
        from PIL import Image  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "overlay OCR needs pytesseract + Pillow (pip install "
            "'dirstral-annotator[ocr]') and a tesseract binary"
        ) from exc

    config = f"--psm {psm}" if psm is not None else ""
    language = default_lang() if lang is None else lang

    def ocr(frame: Path) -> str:
        with Image.open(frame) as img:
            img.load()
            return pytesseract.image_to_string(img, config=config, lang=language)

    return ocr


def default_workers() -> int:
    """How many frames to read at once, from the environment or the host.

    `DIRSTRAL_ANNOTATOR_WORKERS=1` forces the serial path; a value that is not
    a positive integer is ignored rather than silently disabling the pool. The
    unset default divides the core count by the threads one OCR pass already
    uses, because a worker is not one core's worth of work: see
    OCR_THREADS_PER_WORKER.
    """
    raw = os.environ.get(WORKERS_ENV, "").strip()
    if raw:
        try:
            requested = int(raw)
        except ValueError:
            requested = 0
        if requested >= 1:
            return requested
    cores = os.cpu_count() or 1
    return max(1, min(cores // OCR_THREADS_PER_WORKER, MAX_DEFAULT_WORKERS))


def lookahead_for(workers: int) -> int:
    """Size of the dispatch window in front of a pool of `workers`."""
    return max(MIN_LOOKAHEAD, LOOKAHEAD_PER_WORKER * workers)


@dataclass(frozen=True)
class OverlayRead:
    """Everything one band of one sampled frame yielded: text, and when.

    `texts` holds one string per preprocessing pass, in a fixed order, and is
    not deduplicated: the passes disagree, and which one is right depends on
    the frame. Callers merge them under their own rules.
    """

    index: int  # sampled-frame ordinal, 0-based
    timestamp_s: float
    region: Region
    texts: tuple[str, ...] = field(default_factory=tuple)

    def best(self) -> str:
        """The longest pass, for callers that want one string and no rules."""
        return max(self.texts, key=len, default="")


_T = TypeVar("_T")
#: One band's text in, (whatever the caller made of it, hit count) out. The hit
#: count is evidence that this band holds the overlay, and drives the band lock.
Interpreter = Callable[[OverlayRead], tuple[_T, int]]


def any_text(read: OverlayRead) -> tuple[tuple[str, ...], int]:
    """Default interpreter: keep every substantial pass, count the band once.

    Weak on purpose. It cannot tell an overlay from a shop sign in the
    background, so a caller that can distinguish them should say so rather than
    accept this.
    """
    kept = tuple(text for text in read.texts if len(text.strip()) >= MIN_USEFUL_CHARS)
    return kept, 1 if kept else 0


class OverlayReader:
    """Sample a media file and OCR its overlay band, frame by frame.

    Holds no interpretation of its own: `read` takes the caller's, and the only
    thing that crosses back is a hit count. Everything else about a run (which
    band, how many workers, which language) is a constructor argument with a
    general default.
    """

    def __init__(
        self,
        ocr: OcrFn | None = None,
        fps: float = 0.5,
        crop: Region | None = None,  # fraction box; None searches for the band
        regions: Iterable[Region] | None = None,
        workers: int | None = None,  # None: default_workers(); 1: serial
        lang: str | None = None,  # None: LANG_ENV, else the engine's default
        psm: int | None = OCR_PSM,
        name: str = "overlay",  # scratch dir and worker thread prefix
    ):
        self.ocr = ocr if ocr is not None else default_ocr(psm=psm, lang=lang)
        # The language the default adapter was built with, resolved through
        # LANG_ENV. Records what this reader will read in; says nothing about
        # an injected OCR callable, which answers for its own language.
        self.lang = default_lang() if lang is None else lang
        self.psm = psm
        self.fps = fps
        self.crop = crop
        if crop is not None:
            self.regions: tuple[Region, ...] = (tuple(crop),)  # type: ignore[assignment]
        else:
            self.regions = tuple(regions) if regions is not None else CANDIDATE_REGIONS
        self.workers = default_workers() if workers is None else max(1, int(workers))
        self.name = name

    def read(
        self,
        media_path: Path,
        interpret: Interpreter[_T] = any_text,  # type: ignore[assignment]
    ) -> Iterator[tuple[OverlayRead, _T]]:
        """Yield (read, interpretation) for every band the search wanted.

        One frame can yield several bands while the search is still sweeping,
        and none at all on a strided frame. A band that produced hits ends the
        frame: the overlay was found, the other crops are background.

        `interpret` is called on the caller's thread, in frame order, so it may
        accumulate state. The pool sits upstream of it and cannot reorder
        anything: see `_with_lookahead`.
        """
        search = _RegionSearch(self.regions)
        with (
            tempfile.TemporaryDirectory(prefix=f"dirstral-{self.name}-") as tmp,
            _reader(self.ocr, Path(tmp), self.workers, self.name) as pool,
        ):
            frames = iter_frames(media_path, fps=self.fps)
            for i, timestamp, frame in _with_lookahead(frames, search, pool):
                for region in search.regions_for(i):
                    read = OverlayRead(
                        index=i,
                        timestamp_s=timestamp,
                        region=region,
                        texts=tuple(pool.read(i, frame, region)),
                    )
                    value, hits = interpret(read)
                    yield read, value
                    search.record(region, hits)
                    if hits:
                        break  # this band answered; skip the other crops

    def read_text(self, media_path: Path) -> Iterator[OverlayRead]:
        """Every band's text, and nothing else. The no-interpretation path."""
        for read, _ in self.read(media_path):
            yield read


class _RegionSearch:
    """Try every candidate band until one proves itself, then stay on it.

    Sweeping four bands on every frame would cost more OCR than the whole-frame
    pass this replaces, so unlocked sweeps are strided and the winner is locked
    in after a few reads. If the overlay moves (a graphics package change part
    way through a file) the lock releases once the locked band goes quiet.
    """

    RELEASE_AFTER_MISSES = 60

    def __init__(self, regions: tuple[Region, ...]):
        self.regions = regions
        self.locked: Region | None = regions[0] if len(regions) == 1 else None
        self.scores: dict[Region, int] = {}
        self.misses = 0

    def regions_for(self, frame_index: int) -> tuple[Region, ...]:
        if self.locked is not None:
            return (self.locked,)
        if frame_index % SCAN_STRIDE:
            return ()
        return self.regions

    def record(self, region: Region, hits: int) -> None:
        if hits:
            self.scores[region] = self.scores.get(region, 0) + hits
            self.misses = 0
            if self.locked is None and self.scores[region] >= LOCK_AFTER_READS:
                self.locked = region
        elif self.locked == region and len(self.regions) > 1:
            self.misses += 1
            if self.misses >= self.RELEASE_AFTER_MISSES:
                self.locked = None
                self.scores.clear()
                self.misses = 0


def read_band(ocr: OcrFn, frame: Path, region: Region, work: Path) -> list[str]:
    """OCR one band of one frame, once per preprocessing pass.

    The whole of the expensive per-frame work, and a pure function of
    (frame, region): nothing here reads or writes reader state, which is what
    makes it safe to run several at once. Returns the raw strings in pass
    order; merging them is the caller's judgement, not the engine's.
    """
    return [ocr(path) for path in _prepared_crops(frame, region, work)]


class Workspaces:
    """A scratch directory per worker thread.

    Crops are written under fixed names and rewritten per frame, so threads
    sharing one directory would overwrite each other's crop between the write
    and the OCR of it. A directory per thread keeps the fixed names, and with
    them a bounded amount of scratch (a couple of files per worker, rewritten
    every frame, rather than a couple per frame kept until the run ends).
    """

    def __init__(self, root: Path):
        self._root = root
        self._local = threading.local()
        self._lock = threading.Lock()
        self._issued = 0

    def current(self) -> Path:
        work = getattr(self._local, "work", None)
        if work is None:
            with self._lock:
                self._issued += 1
                work = self._root / f"worker-{self._issued}"
            work.mkdir(parents=True, exist_ok=True)
            self._local.work = work
        return work


class _SerialReader:
    """Read one band at a time on the calling thread.

    The reference behaviour, and what `workers=1` selects. `dispatch` and
    `retire` exist so the loop in `OverlayReader.read` is written once.
    """

    workers = 1

    def __init__(self, ocr: OcrFn, work: Path):
        self._ocr = ocr
        self._work = work

    def dispatch(self, index: int, frame: Path, region: Region) -> None:
        pass

    def read(self, index: int, frame: Path, region: Region) -> list[str]:
        return read_band(self._ocr, frame, region, self._work)

    def retire(self, index: int) -> None:
        pass

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        pass


class _PooledReader:
    """Read bands on a worker pool, keyed by frame index so order cannot leak.

    Results are handed back only when the reading loop asks for a specific
    (frame index, region), so the caller sees frames in order whatever order
    the workers happen to finish in. A band that was dispatched and then not
    asked for (the region search changed its mind) is dropped; a band that is
    asked for and was never dispatched is read on the spot.

    The pool is threads, not processes, which the task's own measurement
    justifies: on the pilot fixture 6 threads and 6 processes both returned
    the same reads at the same speed (4.98x versus 4.92x over serial), because
    pytesseract shells out to the `tesseract` binary and Pillow drops the GIL
    around its own work, so the interpreter lock is not the constraint.
    Threads then win on everything else: no pickling constraint on the
    injected OCR callable (the default adapter is a closure), no second copy
    of any model per worker, and a RecognizerUnavailable raised in a worker
    arrives at `future.result()` as itself.
    """

    def __init__(self, ocr: OcrFn, work: Path, workers: int, name: str = "overlay"):
        self.workers = workers
        self._ocr = ocr
        self._workspaces = Workspaces(work)
        self._executor = new_executor(workers, name)
        self._pending: dict[tuple[int, Region], Future[list[str]]] = {}

    def _read_here(self, frame: Path, region: Region) -> list[str]:
        return read_band(self._ocr, frame, region, self._workspaces.current())

    def dispatch(self, index: int, frame: Path, region: Region) -> None:
        key = (index, region)
        if key not in self._pending:
            self._pending[key] = self._executor.submit(self._read_here, frame, region)

    def read(self, index: int, frame: Path, region: Region) -> list[str]:
        pending = self._pending.pop((index, region), None)
        if pending is None:
            return self._read_here(frame, region)
        return pending.result()

    def retire(self, index: int) -> None:
        """Drop dispatches for frames the loop has already passed."""
        for key in [k for k in self._pending if k[0] <= index]:
            self._pending.pop(key).cancel()

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        for pending in self._pending.values():
            pending.cancel()
        self._pending.clear()
        self._executor.shutdown(wait=True, cancel_futures=True)


def new_executor(workers: int, name: str = "overlay") -> Executor:
    return ThreadPoolExecutor(max_workers=workers, thread_name_prefix=name)


def _reader(
    ocr: OcrFn, work: Path, workers: int, name: str = "overlay"
) -> _SerialReader | _PooledReader:
    if workers <= 1:
        return _SerialReader(ocr, work)
    return _PooledReader(ocr, work, workers, name)


def _with_lookahead(
    frames: Iterable[tuple[float, Path]],
    search: _RegionSearch,
    reader: _SerialReader | _PooledReader,
) -> Iterator[tuple[int, float, Path]]:
    """Yield (index, timestamp, frame) while dispatching the reads to come.

    Which band a frame is OCR'd on is sequential state: it depends on what the
    frames before it produced, so the pool cannot simply be handed the whole
    file. It is handed a guess instead, taken from the region search as it
    stands when the frame enters the window, and the reading loop stays
    authoritative: it asks the search which bands it actually wants and takes a
    dispatched result only for exactly those. A guess the search does not take
    is dropped, and a band it wants that was never guessed is read on the spot,
    so the output is the serial one whatever the pool did.

    The whole window is re-dispatched on every step. Dispatching is keyed and
    idempotent, so this costs one dict lookup per frame in the window and
    means the handful of frames already in flight when the search locks onto a
    band get read on the pool rather than one at a time.
    """
    lookahead = lookahead_for(reader.workers)
    window: deque[tuple[int, float, Path]] = deque()
    numbered = enumerate(frames)

    def top_up() -> None:
        while len(window) < lookahead:
            item = next(numbered, None)
            if item is None:
                break
            index, (timestamp, frame) = item
            window.append((index, timestamp, frame))
        for index, _timestamp, frame in window:
            for region in search.regions_for(index):
                reader.dispatch(index, frame, region)

    top_up()
    while window:
        current = window.popleft()
        yield current
        reader.retire(current[0])
        top_up()


_K = TypeVar("_K")
_A = TypeVar("_A")
_R = TypeVar("_R")


def map_in_order(
    executor: Executor,
    fn: Callable[[_A], _R],
    items: Iterable[tuple[_K, _A]],
    lookahead: int,
) -> Iterator[tuple[_K, _R]]:
    """Run `fn` over `items` on the pool, yielding results in *input* order.

    For per-frame work that carries no sequential state, which is the easy half
    of the problem `_with_lookahead` solves: items go in in order, are held in
    a bounded window, and are awaited in that same order, so a worker finishing
    early simply waits its turn and completion order never reaches the caller.
    The window is what keeps a long file from submitting every frame at once.
    """
    window: deque[tuple[_K, Future[_R]]] = deque()
    source = iter(items)

    def top_up() -> None:
        while len(window) < lookahead:
            item = next(source, None)
            if item is None:
                return
            key, arg = item
            window.append((key, executor.submit(fn, arg)))

    top_up()
    while window:
        key, pending = window.popleft()
        yield key, pending.result()
        top_up()


def _prepared_crops(frame: Path, region: Region, work: Path) -> Iterator[Path]:
    """Yield OCR-ready renderings of one band: upscaled greyscale, and the same
    crop hard-thresholded.

    Overlay text is light on dark, which Otsu handles badly when the crop also
    catches a bright background; a fixed threshold recovers the text in exactly
    the frames the grey pass garbles. Both are written to the reader's own temp
    dir rather than beside the frame, because the frame directory is a cache
    other recognizers iterate.
    """
    try:
        from PIL import Image, ImageOps  # required by the OCR extra
    except ImportError as exc:  # pragma: no cover - exercised via the contract test
        # A caller's recognize() must degrade the cascade, not abort it, so a
        # missing Pillow has to arrive as RecognizerUnavailable, not ImportError.
        raise RecognizerUnavailable("Pillow is required for overlay OCR") from exc

    # load() inside the context manager pulls the pixels into memory and lets
    # the descriptor close immediately. This runs at least once per frame, so
    # relying on garbage collection to release it leaks handles across a long
    # media file and can exhaust the process limit.
    with Image.open(frame) as opened:
        opened.load()
        img = opened.copy()
    x, y, w, h = region
    box = (
        max(0, int(x * img.width)),
        max(0, int(y * img.height)),
        min(img.width, int((x + w) * img.width)),
        min(img.height, int((y + h) * img.height)),
    )
    if box[2] <= box[0] or box[3] <= box[1]:
        return
    crop = img.crop(box).convert("L")
    scale = min(MAX_UPSCALE, max(1.0, TARGET_BAND_PX / max(1, crop.height)))
    if scale > 1.0:
        crop = crop.resize(
            (int(crop.width * scale), int(crop.height * scale)), Image.LANCZOS
        )
    crop = ImageOps.autocontrast(crop)

    grey = work / "band-grey.png"
    crop.save(grey)
    yield grey

    binary = work / "band-bin.png"
    crop.point(lambda p: 255 if p > BINARY_THRESHOLD else 0).save(binary)
    yield binary
