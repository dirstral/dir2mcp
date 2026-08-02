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
import re
import subprocess
import tempfile
import threading
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

#: serve runs under ThreadingHTTPServer, so /recognize requests touch this cache
#: concurrently. Without synchronisation two threads race the dict, and eviction
#: can rmtree a directory another thread is still iterating, failing a
#: recognition mid-run. _FRAME_USERS refcounts active iterators so an in-use
#: extraction is never deleted, and _FRAME_INFLIGHT lets a second caller wait
#: for an extraction already running rather than start a duplicate hour-long
#: decode of the same media.
_FRAME_LOCK = threading.Condition()
_FRAME_USERS: dict[tuple[str, int, int, float], int] = {}
_FRAME_INFLIGHT: set[tuple[str, int, int, float]] = set()


#: Exactly the files ffmpeg's `frame-%08d.jpg` pattern produces, and nothing
#: else. Recognizers write siblings into this directory (JerseyRecognizer saves
#: crops as `frame-<n>-p<x>x<y>.jpg`), and now that the directory is SHARED
#: across the cascade a loose `frame-*.jpg` glob would feed one recognizer's
#: crops to the next as if they were sampled frames: wrong images, and
#: timestamps inflated by the extra entries (a 900s clip yielded cues out to
#: 6446s). Each iterator was previously handed a private directory, so this
#: only became reachable when extraction started being reused.
_SAMPLED_FRAME_RE = re.compile(r"^frame-\d+\.jpg$")


def _sampled_frames(directory: Path) -> list[Path]:
    return sorted(p for p in directory.glob("frame-*.jpg")
                  if _SAMPLED_FRAME_RE.match(p.name))


def _evict_frame_dir(path: Path) -> None:
    shutil.rmtree(path, ignore_errors=True)


def _evict_locked() -> None:
    """Trim to the cap, skipping entries an iterator still holds.

    Caller must hold _FRAME_LOCK. Over-cap is preferable to deleting frames out
    from under a running recognizer; the entry is reclaimed once its last
    reader finishes.
    """
    while len(_FRAME_CACHE) > _FRAME_CACHE_MAX:
        victim = next((k for k in _FRAME_CACHE if not _FRAME_USERS.get(k)), None)
        if victim is None:
            return
        _evict_frame_dir(_FRAME_CACHE.pop(victim))


def _cleanup_frame_dirs() -> None:
    with _FRAME_LOCK:
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

    with _FRAME_LOCK:
        # Wait out an extraction of the same media already running in another
        # thread instead of starting a duplicate decode.
        while key in _FRAME_INFLIGHT:
            _FRAME_LOCK.wait()
        cached = _FRAME_CACHE.get(key)
        if cached is None:
            _FRAME_INFLIGHT.add(key)

    if cached is None:
        try:
            tmp = Path(tempfile.mkdtemp(prefix="dirstral-frames-"))
        except BaseException:
            # The in-flight marker is already set. Failing to clear it here (a
            # full disk, a bad TMPDIR) would leave every later caller for this
            # key blocked forever in the wait loop above.
            with _FRAME_LOCK:
                _FRAME_INFLIGHT.discard(key)
                _FRAME_LOCK.notify_all()
            raise
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
        except BaseException as exc:
            # A failed extraction must leave nothing behind: not the partially
            # written directory, not a cache entry, and not a marker that would
            # strand another thread waiting in the loop above.
            _evict_frame_dir(tmp)
            with _FRAME_LOCK:
                _FRAME_INFLIGHT.discard(key)
                _FRAME_LOCK.notify_all()
            if isinstance(exc, subprocess.TimeoutExpired):
                raise RuntimeError(
                    f"ffmpeg timed out after {timeout:.0f}s on {media_path}"
                ) from exc
            if isinstance(exc, FileNotFoundError):
                raise RecognizerUnavailable(f"ffmpeg not found ({ffmpeg})") from exc
            if isinstance(exc, subprocess.CalledProcessError):
                raise RuntimeError(
                    f"ffmpeg failed on {media_path}: "
                    f"{exc.stderr.decode(errors='replace')[-500:]}"
                ) from exc
            raise
        with _FRAME_LOCK:
            _FRAME_CACHE[key] = tmp
            _FRAME_INFLIGHT.discard(key)
            _FRAME_LOCK.notify_all()
            cached = tmp

    with _FRAME_LOCK:
        _FRAME_CACHE.move_to_end(key)
        _FRAME_USERS[key] = _FRAME_USERS.get(key, 0) + 1
        _evict_locked()

    try:
        for i, frame in enumerate(_sampled_frames(cached)):
            # ffmpeg's fps filter emits frame N at timestamp N/fps.
            yield i / fps, frame
    finally:
        # Release the hold whether the caller exhausted the iterator or closed
        # it early, then retry the eviction this reader may have been blocking.
        with _FRAME_LOCK:
            remaining = _FRAME_USERS.get(key, 1) - 1
            if remaining > 0:
                _FRAME_USERS[key] = remaining
            else:
                _FRAME_USERS.pop(key, None)
            _evict_locked()
            _FRAME_LOCK.notify_all()
