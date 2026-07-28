"""Recognizer interface and shared video-frame plumbing.

Every recognizer turns one media file into a list of Cues. Heavy CV
dependencies (OpenCV, OCR engines, face models) are injected as callables
so the package imports, tests, and runs the metadata-driven recognizers
(play-by-play) on a machine with none of them installed.
"""

from __future__ import annotations

import subprocess
import tempfile
from collections.abc import Iterator
from pathlib import Path
from typing import Protocol

from ..model import Cue


class Recognizer(Protocol):
    #: stable source tag recorded on every cue ("scorebug", "face", ...)
    name: str

    def recognize(self, media_path: Path) -> list[Cue]: ...


class RecognizerUnavailable(RuntimeError):
    """Raised at construction when a recognizer's backend (model, binary,
    network resource) is missing. The CLI reports and skips it; the rest of
    the cascade still runs."""


def iter_frames(
    media_path: Path, fps: float = 1.0, ffmpeg: str = "ffmpeg"
) -> Iterator[tuple[float, Path]]:
    """Yield (timestamp_seconds, jpeg_path) sampled at `fps` via ffmpeg.

    Frames land in a temp dir that is deleted when the iterator is
    exhausted or closed, so callers must consume (or copy) eagerly.
    """
    if fps <= 0:
        raise ValueError("fps must be positive")
    with tempfile.TemporaryDirectory(prefix="dirstral-frames-") as tmp:
        out = Path(tmp) / "frame-%08d.jpg"
        cmd = [
            ffmpeg, "-hide_banner", "-loglevel", "error",
            "-i", str(media_path),
            "-vf", f"fps={fps}",
            "-q:v", "3",
            str(out),
        ]
        try:
            subprocess.run(cmd, check=True, capture_output=True, timeout=300)
        except subprocess.TimeoutExpired as exc:
            raise RuntimeError(f"ffmpeg timed out after 300s on {media_path}") from exc
        except FileNotFoundError as exc:
            raise RecognizerUnavailable(f"ffmpeg not found ({ffmpeg})") from exc
        except subprocess.CalledProcessError as exc:
            raise RuntimeError(
                f"ffmpeg failed on {media_path}: {exc.stderr.decode(errors='replace')[-500:]}"
            ) from exc
        for i, frame in enumerate(sorted(Path(tmp).glob("frame-*.jpg"))):
            # ffmpeg's fps filter emits frame N at timestamp N/fps.
            yield i / fps, frame
