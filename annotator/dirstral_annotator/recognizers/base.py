"""Recognizer interface and shared video-frame plumbing.

Every recognizer turns one media file into a list of Cues. Heavy CV
dependencies (OpenCV, OCR engines, face models) are injected as callables
so the package imports, tests, and runs the metadata-driven recognizers
(play-by-play) on a machine with none of them installed.
"""

from __future__ import annotations

import atexit
import json
import shutil
import subprocess
import tempfile
from collections import OrderedDict
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


#: Extractions already performed this process, keyed by
#: (resolved path, mtime, size, fps). Every recognizer in the cascade samples
#: the SAME media at the SAME fps, and each used to trigger its own full
#: decode: a three hour broadcast cost three hour-long passes for one run.
#: Bounded because `serve` is long lived: it answers /recognize for many
#: different media, and an unbounded memo would accumulate a full JPEG set per
#: file until the process exits, filling the disk. A small cap is enough for the
#: cascade (every recognizer in one run shares a single key) while letting an
#: earlier file's frames be reclaimed.
_FRAME_CACHE_MAX = 4
_FRAME_CACHE: "OrderedDict[tuple[str, int, int, float], Path]" = OrderedDict()


def _evict_frame_dir(path: Path) -> None:
    shutil.rmtree(path, ignore_errors=True)


def _cleanup_frame_dirs() -> None:
    while _FRAME_CACHE:
        _evict_frame_dir(_FRAME_CACHE.popitem(last=False)[1])


atexit.register(_cleanup_frame_dirs)


def probe_duration_s(media_path: Path, ffprobe: str = "ffprobe") -> float | None:
    """Media duration in seconds, or None when ffprobe cannot report it.

    TypeError is caught alongside the rest: ffprobe reports `"duration": null`
    for some containers, and float(None) would otherwise escape as an unhandled
    error and abort an extraction that should simply fall back.
    """
    try:
        out = subprocess.run(
            [ffprobe, "-v", "error", "-show_entries", "format=duration",
             "-of", "json", str(media_path)],
            check=True, capture_output=True, timeout=60,
        ).stdout
        return float(json.loads(out)["format"]["duration"])
    except (subprocess.SubprocessError, OSError, KeyError, TypeError, ValueError):
        return None


def _extract_timeout_s(media_path: Path, ffprobe: str) -> float:
    """Budget for the extraction pass, scaled to the media.

    A fixed ceiling cannot work: decoding is proportional to duration, so any
    constant is either far too small for real footage or meaningless for a
    clip. A three hour broadcast needs roughly an hour of decode, which the
    previous hardcoded 300s could never accommodate. Allow 4x realtime, with a
    floor for short media, and treat an unknown duration as "no limit" rather
    than guessing a number that would abort a legitimate run.
    """
    duration = probe_duration_s(media_path, ffprobe=ffprobe)
    if duration is None:
        return float("inf")
    return max(600.0, duration * 4.0)


def iter_frames(
    media_path: Path, fps: float = 1.0, ffmpeg: str = "ffmpeg",
    ffprobe: str = "ffprobe",
) -> Iterator[tuple[float, Path]]:
    """Yield (timestamp_seconds, jpeg_path) sampled at `fps` via ffmpeg.

    Frames are extracted once per (media, fps) and reused by every recognizer
    in the process; they are removed at interpreter exit, not when the iterator
    is exhausted. Callers must therefore not assume the directory disappears
    mid-run, and must still copy anything they need beyond process lifetime.
    """
    if fps <= 0:
        raise ValueError("fps must be positive")

    try:
        st = media_path.stat()
    except OSError as exc:
        raise RuntimeError(f"cannot stat {media_path}: {exc}") from exc
    key = (str(media_path.resolve()), st.st_mtime_ns, st.st_size, fps)

    cached = _FRAME_CACHE.get(key)
    if cached is not None:
        _FRAME_CACHE.move_to_end(key)
    else:
        tmp = Path(tempfile.mkdtemp(prefix="dirstral-frames-"))
        out = tmp / "frame-%08d.jpg"
        cmd = [
            ffmpeg, "-hide_banner", "-loglevel", "error",
            "-i", str(media_path),
            "-vf", f"fps={fps}",
            "-q:v", "3",
            str(out),
        ]
        timeout = _extract_timeout_s(media_path, ffprobe)
        try:
            subprocess.run(cmd, check=True, capture_output=True,
                           timeout=None if timeout == float("inf") else timeout)
        except subprocess.TimeoutExpired as exc:
            _evict_frame_dir(tmp)
            raise RuntimeError(
                f"ffmpeg timed out after {timeout:.0f}s on {media_path}"
            ) from exc
        except FileNotFoundError as exc:
            _evict_frame_dir(tmp)
            raise RecognizerUnavailable(f"ffmpeg not found ({ffmpeg})") from exc
        except subprocess.CalledProcessError as exc:
            _evict_frame_dir(tmp)
            raise RuntimeError(
                f"ffmpeg failed on {media_path}: {exc.stderr.decode(errors='replace')[-500:]}"
            ) from exc
        except BaseException:
            # Anything else (KeyboardInterrupt included) must also not leave a
            # partially written directory behind until interpreter exit.
            _evict_frame_dir(tmp)
            raise
        _FRAME_CACHE[key] = tmp
        while len(_FRAME_CACHE) > _FRAME_CACHE_MAX:
            _evict_frame_dir(_FRAME_CACHE.popitem(last=False)[1])
        cached = tmp

    for i, frame in enumerate(sorted(cached.glob("frame-*.jpg"))):
        # ffmpeg's fps filter emits frame N at timestamp N/fps.
        yield i / fps, frame
