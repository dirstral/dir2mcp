"""Jersey-number recognizer: OCR digits on tracked player crops.

Person detection is injected as `(jpeg_path) -> [bbox]` (default adapter:
ultralytics YOLO when installed) and reuses the scorebug OCR callable on
each crop. Numbers resolve to players via the roster; a number nobody on
the roster wears is dropped rather than guessed.

Weakest signal in the cascade (low resolution, motion blur — see the
SoccerNet jersey benchmarks), so its confidence ceiling is deliberately
modest and it exists to corroborate, not to decide.
"""

from __future__ import annotations

import re
from collections.abc import Callable
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, iter_frames
from .scorebug import OcrFn, collapse_sightings, default_ocr

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
    ):
        self.roster = roster
        self.detector = detector if detector is not None else default_detector()
        self.ocr = ocr if ocr is not None else default_ocr()
        self.fps = fps

    def recognize(self, media_path: Path) -> list[Cue]:
        sightings: list[tuple[float, str, float]] = []
        for t, frame in iter_frames(media_path, fps=self.fps):
            for bbox in self.detector(frame):
                crop = _crop(frame, bbox)
                text = self.ocr(crop)
                for num in NUMBER_RE.findall(text):
                    player = self.roster.by_number(num)
                    if player:
                        sightings.append((t, player.id, CONFIDENCE_CEILING))
        return collapse_sightings(
            sightings, source=self.name, event="appearance", frame_gap=1.0 / self.fps
        )


def _crop(frame: Path, bbox: Bbox) -> Path:
    from PIL import Image  # required transitively by the OCR extra

    x, y, w, h = bbox
    img = Image.open(frame)
    out = frame.with_name(f"{frame.stem}-p{x}x{y}.jpg")
    img.crop((max(0, x), max(0, y), x + w, y + h)).save(out)
    return out
