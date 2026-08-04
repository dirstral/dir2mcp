"""Jersey-number recognizer: OCR digits on tracked player crops.

Person detection is injected as `(jpeg_path) -> [bbox]` (default adapter:
ultralytics YOLO when installed) and reuses the overlay reader's OCR callable
on each crop. Numbers resolve to players via the roster; a number nobody on
the roster wears is dropped rather than guessed.

Weakest signal in the cascade (low resolution, motion blur; see the
SoccerNet jersey benchmarks), so its confidence ceiling is deliberately
modest and it exists to corroborate, not to decide.

Frames are read on a worker pool for the same reason the overlay reader's are:
the per-frame detect-crop-OCR is the whole cost, it is a pure function of the
frame, and a broadcast has thousands of them. The pool plumbing (worker count,
per-thread scratch, in-order results) is shared with the overlay reader rather
than reimplemented here; what is left below is the crop-and-read itself.
"""

from __future__ import annotations

import re
import tempfile
import threading
from collections.abc import Callable, Iterable, Iterator
from pathlib import Path
from typing import Self

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, collapse_sightings, iter_frames
from .overlay import (
    OcrFn,
    Workspaces,
    _new_executor,
    default_ocr,
    default_workers,
    lookahead_for,
    map_in_order,
)

Bbox = tuple[int, int, int, int]  # x, y, w, h
DetectFn = Callable[[Path], list[Bbox]]

NUMBER_RE = re.compile(r"(?<!\d)(\d{1,2})(?!\d)")
CONFIDENCE_CEILING = 0.6


def default_detector() -> DetectFn:
    try:
        from ultralytics import YOLO  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "jersey recognition needs ultralytics (pip install "
            "'dirstral-annotator[jersey]')"
        ) from exc

    model = YOLO("yolov8n.pt")

    def detect(frame: Path) -> list[Bbox]:
        boxes = []
        for result in model(str(frame), classes=[0], verbose=False):  # class 0: person
            for box in result.boxes.xywh.tolist():
                cx, cy, w, h = box
                boxes.append((int(cx - w / 2), int(cy - h / 2), int(w), int(h)))
        return boxes

    return detect


class JerseyRecognizer:
    name = "jersey"

    def __init__(
        self,
        roster: Roster,
        detector: DetectFn | None = None,
        ocr: OcrFn | None = None,
        fps: float = 0.5,
        workers: int | None = None,  # None: default_workers(); 1: serial
        lang: str | None = None,  # OCR language; None: the shared default
    ):
        self.roster = roster
        self.detector = detector if detector is not None else default_detector()
        self.ocr = ocr if ocr is not None else default_ocr(lang=lang)
        self.fps = fps
        self.workers = default_workers() if workers is None else max(1, int(workers))
        # An injected detector is used as given: the caller supplied the
        # instance, and knows whether sharing it is safe. The default adapter
        # is rebuilt per worker thread instead; see _Detectors.
        self._new_detector: Callable[[], DetectFn] = (
            default_detector if detector is None else (lambda: self.detector)
        )

    def recognize(self, media_path: Path) -> list[Cue]:
        sightings: list[tuple[float, str, float]] = []
        with (
            tempfile.TemporaryDirectory(prefix="dirstral-jersey-") as tmp,
            self._reader(Path(tmp)) as reader,
        ):
            for t, numbers in reader.read(iter_frames(media_path, fps=self.fps)):
                for num in numbers:
                    player = self.roster.by_number(num)
                    if player:
                        sightings.append((t, player.id, CONFIDENCE_CEILING))
        return collapse_sightings(
            sightings, source=self.name, event="appearance", frame_gap=1.0 / self.fps
        )

    def _reader(self, work: Path) -> _SerialReader | _PooledReader:
        if self.workers <= 1:
            return _SerialReader(self.detector, self.ocr, work)
        detectors = _Detectors(self._new_detector, seed=self.detector)
        return _PooledReader(detectors, self.ocr, work, self.workers, self.name)


def read_numbers(detect: DetectFn, ocr: OcrFn, frame: Path, work: Path) -> list[str]:
    """Every jersey number one frame shows, in detection order.

    The whole of the expensive per-frame work, and a pure function of the
    frame: no recognizer state is read or written here, which is what makes
    it safe to run several at once.
    """
    numbers: list[str] = []
    for bbox in detect(frame):
        numbers += NUMBER_RE.findall(ocr(_crop(frame, bbox, work)))
    return numbers


class _Detectors:
    """One person detector per worker thread.

    An ultralytics model is not documented thread safe (a predictor hangs
    per-call state off the model object), so threads sharing one instance can
    read each other's boxes. yolov8n is a few megabytes, so a copy per worker
    is cheap next to being wrong. The instance built in the constructor (which
    is what makes a missing ultralytics fail fast, as RecognizerUnavailable at
    construction rather than mid-run) is handed to the first thread to ask
    rather than left idle.

    Construction is serialised. The default adapter's first call fetches the
    weights file if it is not already on disk, and several workers racing that
    would write the same path at once; serialising also keeps the workers from
    loading a model each at the same moment, which is the one point in the run
    where they would all want memory together.
    """

    def __init__(self, factory: Callable[[], DetectFn], seed: DetectFn | None = None):
        self._factory = factory
        self._spare = [seed] if seed is not None else []
        self._lock = threading.Lock()
        self._local = threading.local()

    def current(self) -> DetectFn:
        detect = getattr(self._local, "detect", None)
        if detect is None:
            with self._lock:
                detect = self._spare.pop() if self._spare else self._factory()
            self._local.detect = detect
        return detect


class _SerialReader:
    """Read one frame at a time on the calling thread: the reference path."""

    workers = 1

    def __init__(self, detect: DetectFn, ocr: OcrFn, work: Path):
        self._detect = detect
        self._ocr = ocr
        self._work = work

    def read(
        self, frames: Iterable[tuple[float, Path]]
    ) -> Iterator[tuple[float, list[str]]]:
        for t, frame in frames:
            yield t, read_numbers(self._detect, self._ocr, frame, self._work)

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        pass


class _PooledReader:
    """Read frames on a worker pool, yielding them back in frame order.

    Nothing about this recognizer is sequential (unlike the overlay reader's
    region search), so ordering needs no reconciliation and `map_in_order` is
    the whole of it: frames go in in order, are held in a bounded window, and
    are awaited in that same order, so a worker finishing early simply waits
    its turn and the sighting list, and with it every cue, is the serial one.

    The pool is threads, not processes, because the measurement says the GIL
    is not the constraint here: pytesseract shells out to the `tesseract`
    binary and ultralytics spends its time inside torch, both of which drop
    the lock. Threads also keep the injected detector and OCR callables free
    of any pickling constraint, which the default adapters (closures over a
    loaded model) could not satisfy.
    """

    def __init__(
        self,
        detectors: _Detectors,
        ocr: OcrFn,
        work: Path,
        workers: int,
        name: str = "jersey",
    ):
        self.workers = workers
        self._detectors = detectors
        self._ocr = ocr
        self._workspaces = Workspaces(work)
        self._executor = _new_executor(workers, name)

    def _read_here(self, frame: Path) -> list[str]:
        return read_numbers(
            self._detectors.current(), self._ocr, frame, self._workspaces.current()
        )

    def read(
        self, frames: Iterable[tuple[float, Path]]
    ) -> Iterator[tuple[float, list[str]]]:
        return map_in_order(
            self._executor, self._read_here, frames, lookahead_for(self.workers)
        )

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        self._executor.shutdown(wait=True, cancel_futures=True)


def _crop(frame: Path, bbox: Bbox, work: Path) -> Path:
    try:
        from PIL import Image  # required transitively by the OCR extra
    except ImportError as exc:  # pragma: no cover - exercised via the contract test
        # recognize() must degrade the cascade, not abort it, so a missing
        # Pillow has to arrive as RecognizerUnavailable rather than ImportError.
        raise RecognizerUnavailable("Pillow is required for jersey OCR") from exc

    x, y, w, h = bbox
    out = work / "crop.jpg"
    with Image.open(frame) as opened:
        opened.load()
        opened.crop((max(0, x), max(0, y), x + w, y + h)).save(out)
    return out
