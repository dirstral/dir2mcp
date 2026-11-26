"""The served shot detector (dirstral-spec SPEC §8.10.3): `POST /shots`.

dir2mcp with `media.shots.detector: serve` sends `{"path": "<abs media path>"}`
and stores the returned cut times as shots. The answer is
`{"detector": {"name", "version"}, "cuts": [seconds, ...]}`
(spec/shots/response.schema.json).

The detector is TransNetV2 (`transnetv2-pytorch`), a learned shot detector. On
a 71-film news sample it found the cuts that a pixel difference misses: cuts
inside an inset or split-screen picture and cuts between dark shots (design
0007). It needs the `shots` extra (torch); the rest of this module is
stdlib-only, so the core and its tests run anywhere.

Frames stream from ffmpeg at a fixed 25 fps and 48x27 pixels, the input size
TransNetV2 was trained on. A three-hour broadcast is 270,000 such frames, about
1 GB, so they are never held at once: the windowing below keeps 100 frames.
"""

from __future__ import annotations

import subprocess
import tempfile
import threading
from collections.abc import Callable, Iterable, Iterator, Sequence

FPS = 25.0
WIDTH, HEIGHT = 48, 27
FRAME_BYTES = WIDTH * HEIGHT * 3
THRESHOLD = 0.5

# TransNetV2 reads windows of 100 frames and is reliable for the middle 50; the
# window then moves on by 50. The first window starts 25 frames before the
# video (the first frame repeated), so every real frame lands in a middle.
WINDOW, KEEP, LEAD = 100, 50, 25

Predictor = Callable[[Sequence[bytes]], Sequence[float]]


def iter_frames(path: str, ffmpeg: str = "ffmpeg") -> Iterator[bytes]:
    """Decode `path` to raw 48x27 rgb24 frames at a fixed 25 fps.

    A fixed rate makes frame i lie at i / 25 seconds, also for a source with a
    variable frame rate. Raises RuntimeError when ffmpeg fails.
    """
    cmd = [ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", path,
           "-an", "-sn", "-dn", "-vf", f"fps={FPS:g},scale={WIDTH}:{HEIGHT}",
           "-pix_fmt", "rgb24", "-f", "rawvideo", "-"]
    # stderr goes to a file, not a pipe: damaged media can write enough
    # decoder errors to fill a pipe, and then ffmpeg would block before it
    # writes more frames or exits, and so would this reader.
    with tempfile.TemporaryFile() as errfile:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=errfile)
        try:
            assert proc.stdout is not None
            while True:
                frame = proc.stdout.read(FRAME_BYTES)
                if len(frame) < FRAME_BYTES:
                    break
                yield frame
        finally:
            if proc.stdout is not None:
                proc.stdout.close()
            if proc.wait() != 0:
                errfile.seek(0)
                # Only the last line: the full text can name local paths.
                lines = errfile.read().decode("utf-8", "replace").strip().splitlines()
                last = lines[-1] if lines else ""
                raise RuntimeError(f"ffmpeg exited with status {proc.returncode}: {last[:300]}")


def predict_stream(frames: Iterable[bytes], predict: Predictor) -> list[float]:
    """Run a windowed predictor over a frame stream; one probability per frame.

    `predict` takes 100 frames and returns 100 probabilities. Each real frame
    takes its value from the window in which it sits among the middle 50
    frames (positions 25 to 74), as in the TransNetV2 reference code. Memory
    stays at one window.
    """
    out: list[float] = []
    buf: list[bytes] = []
    total = 0
    for frame in frames:
        if not buf:
            buf = [frame] * LEAD
        buf.append(frame)
        total += 1
        if len(buf) == WINDOW:
            out.extend(predict(buf)[LEAD:LEAD + KEEP])
            buf = buf[KEEP:]
    if total == 0:
        return []
    while len(out) < total:
        buf = buf + [buf[-1]] * (WINDOW - len(buf))
        out.extend(predict(buf)[LEAD:LEAD + KEEP])
        buf = buf[KEEP:]
    return out[:total]


def predictions_to_cuts(predictions: Sequence[float], threshold: float = THRESHOLD,
                        fps: float = FPS) -> list[float]:
    """Turn per-frame transition probabilities into cut times in seconds.

    The scene rule of `TransNetV2.predictions_to_scenes`: frames above the
    threshold are a transition, and a new shot starts at the first frame after
    a transition. The first shot starts at 0, so it is not a cut.
    """
    cuts: list[float] = []
    previous = False
    for i, p in enumerate(predictions):
        current = p > threshold
        if previous and not current:
            cuts.append(round(i / fps, 3))
        previous = current
    return cuts


class TransNetV2Shots:
    """TransNetV2 on the GPU when there is one, else on the CPU.

    One model serves every request. The lock covers only the model call: the
    ffmpeg decode of each request runs in parallel, and only the GPU work takes
    turns. The decode is most of the time, so several requests at once finish
    sooner than one after another.
    """

    def __init__(self, device: str | None = None, threshold: float = THRESHOLD,
                 ffmpeg: str = "ffmpeg") -> None:
        try:
            import importlib.metadata as md

            import torch
            from transnetv2_pytorch import TransNetV2
        except ImportError as exc:  # pragma: no cover - depends on the extra
            raise RuntimeError(
                "shot detection needs the `shots` extra: pip install 'dirstral-annotator[shots]'"
            ) from exc
        self._torch = torch
        self.device = device or ("cuda" if torch.cuda.is_available() else "cpu")
        self._model = TransNetV2(device=self.device)
        # The package turns deterministic algorithms on globally; turn them
        # off so the other recognizers in this process keep their speed.
        torch.use_deterministic_algorithms(False)
        self.threshold = threshold
        self.ffmpeg = ffmpeg
        self._lock = threading.Lock()
        self.version = (f"transnetv2-pytorch {md.version('transnetv2-pytorch')}; "
                        f"threshold {threshold:g}; fps {FPS:g}")

    def identity(self) -> dict:
        return {"name": "transnetv2", "version": self.version}

    def _predict(self, window: Sequence[bytes]) -> list[float]:
        torch = self._torch
        raw = b"".join(window)
        frames = torch.frombuffer(bytearray(raw), dtype=torch.uint8).reshape(len(window), HEIGHT, WIDTH, 3)
        with self._lock, torch.no_grad():
            single, _ = self._model.predict_raw(frames.unsqueeze(0).to(self.device))
            return single[0, :, 0].float().cpu().tolist()

    def cuts(self, path: str) -> list[float]:
        predictions = predict_stream(iter_frames(path, self.ffmpeg), self._predict)
        return predictions_to_cuts(predictions, self.threshold)
