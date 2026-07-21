"""Scorebug recognizer: OCR the broadcast overlay.

The overlay names the current batter (and often the pitcher) during every
at-bat, which makes this the workhorse signal for broadcast-era footage.
The OCR engine is injected as a callable `(jpeg_path) -> str`; the default
adapter uses pytesseract when installed.

Consecutive frames resolving to the same player collapse into one cue
spanning the run — the overlay persisting for 20 seconds is one at-bat
sighting, not 20 observations (which would also defeat fusion's per-source
confidence cap).
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, iter_frames

OcrFn = Callable[[Path], str]

# A run of frames may drop a detection for a beat (OCR flicker); allow this
# many seconds of gap before closing the run.
RUN_GAP_S = 3.0


def default_ocr() -> OcrFn:
    try:
        import pytesseract  # type: ignore
        from PIL import Image  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "scorebug OCR needs pytesseract + Pillow (pip install "
            "'dirstral-annotator[ocr]') and a tesseract binary"
        ) from exc

    def ocr(frame: Path) -> str:
        return pytesseract.image_to_string(Image.open(frame))

    return ocr


class ScorebugRecognizer:
    name = "scorebug"

    def __init__(
        self,
        roster: Roster,
        ocr: OcrFn | None = None,
        fps: float = 0.5,
        crop=None,  # optional (x, y, w, h) fraction box; None = full frame
    ):
        self.roster = roster
        self.ocr = ocr if ocr is not None else default_ocr()
        self.fps = fps
        self.crop = crop

    def recognize(self, media_path: Path) -> list[Cue]:
        sightings: list[tuple[float, str, float]] = []  # (t, player_id, conf)
        for t, frame in iter_frames(media_path, fps=self.fps):
            frame = self._cropped(frame)
            text = self.ocr(frame)
            for line in text.splitlines():
                hit = self.roster.resolve_name(line)
                if hit:
                    player, conf = hit
                    sightings.append((t, player.id, conf))
        return collapse_sightings(
            sightings, source=self.name, event="at_bat", frame_gap=1.0 / self.fps
        )

    def _cropped(self, frame: Path) -> Path:
        if self.crop is None:
            return frame
        from PIL import Image  # optional dep; only reached when crop is set

        img = Image.open(frame)
        x, y, w, h = self.crop
        px = (
            int(x * img.width), int(y * img.height),
            int((x + w) * img.width), int((y + h) * img.height),
        )
        out = frame.with_name(frame.stem + "-crop.jpg")
        img.crop(px).save(out)
        return out


def collapse_sightings(
    sightings: list[tuple[float, str, float]],
    source: str,
    event: str,
    frame_gap: float,
) -> list[Cue]:
    """Collapse per-frame (t, player_id, confidence) hits into per-run cues.

    Shared by every frame-sampling recognizer (scorebug, jersey, face).
    A run of consecutive sightings of one player becomes one cue spanning
    first..last sighting (extended by one frame interval so a single-frame
    sighting still has nonzero duration); confidence is the run's max.
    """
    runs: dict[str, list[tuple[float, float, float, float]]] = {}
    for t, pid, conf in sorted(sightings):
        pruns = runs.setdefault(pid, [])
        if pruns and t - pruns[-1][1] <= frame_gap + RUN_GAP_S:
            s, _, c, first = pruns[-1]
            pruns[-1] = (s, t, max(c, conf), first)
        else:
            pruns.append((t, t, conf, t))

    cues = []
    for pid, pruns in runs.items():
        for start, end, conf, _ in pruns:
            cues.append(
                Cue(
                    source=source,
                    start_s=start,
                    end_s=end + frame_gap,
                    event=event,
                    entity_ids=(pid,),
                    confidence=round(conf, 4),
                )
            )
    cues.sort(key=lambda c: (c.start_s, c.entity_ids))
    return cues
