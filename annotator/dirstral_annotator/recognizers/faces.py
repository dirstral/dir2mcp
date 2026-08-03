"""Face recognizer: match sampled frames against the roster image bank.

The embedding backend is injected as a callable `(jpeg_path) -> list of
(embedding, bbox)`; the default adapter uses InsightFace (buffalo_l) when
installed. Enrollment averages each player's bank embeddings into one
centroid; recognition is cosine similarity against centroids with an
accept threshold.

Honest expectations (pilot proposal): faces are small and occluded in
broadcast footage — this signal earns its keep on close-ups, dugout shots
and pre-overlay archival material, and is fused with, never trusted over,
scorebug/play-by-play.
"""

from __future__ import annotations

import logging
import math
import os
from collections.abc import Callable
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, iter_frames
from .scorebug import collapse_sightings

Embedding = list[float]
# (frame jpeg) -> [(embedding, (x, y, w, h))]
EmbedFn = Callable[[Path], list[tuple[Embedding, tuple[int, int, int, int]]]]

IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".webp"}

# Cosine-similarity accept threshold for ArcFace-class embeddings: ~0.35-0.45
# separates same/different identity on in-the-wild data; the pilot's phase-1
# eval is what tunes this per-corpus.
SIM_THRESHOLD = 0.40


#: Environment override for the ONNX execution provider: "cuda", "cpu", or
#: "auto" (the default). Explicit beats inferred when an operator needs to keep
#: a shared GPU free, and "cuda" makes a misconfigured host fail loudly instead
#: of quietly costing hours.
PROVIDER_ENV = "DIRSTRAL_FACE_PROVIDER"


def select_face_providers(requested: str | None = None) -> tuple[list[str], int]:
    """Return the ONNX provider list and insightface ctx_id to use.

    Auto-detection prefers CUDA when onnxruntime actually offers it. Measured on
    an NVIDIA A2 with buffalo_l at 640x640: 0.154 s/frame on GPU against
    1.340 s/frame on CPU, identical detections. Over a 6118 frame game that is
    16 minutes rather than 2 hours 17, which is the difference between iterating
    on recognizer accuracy and not.

    The old behaviour hardcoded CPU with a comment that pilot sampling rates did
    not need a GPU. That was measurably wrong, and worse, invisible: a CPU run on
    a GPU host looked identical to a correct one.
    """
    want = (requested or os.environ.get(PROVIDER_ENV) or "auto").strip().lower()
    if want == "cpu":
        return ["CPUExecutionProvider"], -1

    available: list[str] = []
    try:
        import onnxruntime as ort  # type: ignore

        available = list(ort.get_available_providers())
    except ImportError:
        available = []

    # Availability is necessary but not sufficient: onnxruntime lists the CUDA
    # provider even when its CUDA/cuDNN libraries fail to load, and only reports
    # the fallback once a session is created. Callers log what they actually got.
    cuda_offered = "CUDAExecutionProvider" in available
    if want == "cuda":
        if not cuda_offered:
            raise RecognizerUnavailable(
                f"{PROVIDER_ENV}=cuda but onnxruntime offers no CUDAExecutionProvider; "
                "install onnxruntime-gpu and its matching CUDA/cuDNN wheels"
            )
        return ["CUDAExecutionProvider", "CPUExecutionProvider"], 0
    if cuda_offered:
        return ["CUDAExecutionProvider", "CPUExecutionProvider"], 0
    return ["CPUExecutionProvider"], -1


def _session_providers(app: object) -> list[str]:
    """Providers the underlying ONNX sessions actually bound, best effort."""
    try:
        models = getattr(app, "models", {}) or {}
        for model in models.values():
            session = getattr(model, "session", None)
            if session is not None:
                return list(session.get_providers())
    except Exception:  # pragma: no cover - diagnostics must never break a run
        pass
    return []


def default_embedder(provider: str | None = None) -> EmbedFn:
    try:
        import cv2  # type: ignore
        from insightface.app import FaceAnalysis  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "face recognition needs insightface + opencv "
            "(pip install 'dirstral-annotator[face]')"
        ) from exc

    providers, ctx_id = select_face_providers(provider)
    app = FaceAnalysis(name="buffalo_l", providers=providers)
    app.prepare(ctx_id=ctx_id)

    # Report what the session actually bound, not what was requested. A CUDA
    # provider that fails to load silently degrades to CPU, which is the failure
    # mode that cost this pilot a full eval run before anyone noticed.
    actual = _session_providers(app)
    logging.getLogger(__name__).info(
        "face recognition provider: %s (requested %s)",
        actual or "unknown", providers[0],
    )
    if "CUDAExecutionProvider" in providers and "CUDAExecutionProvider" not in actual:
        logging.getLogger(__name__).warning(
            "CUDA was requested but the session bound %s; face recognition will "
            "run roughly 8x slower. Check that onnxruntime-gpu matches the "
            "installed CUDA major version and that its libraries are on "
            "LD_LIBRARY_PATH.", actual or "CPU",
        )

    def embed(frame: Path) -> list[tuple[Embedding, tuple[int, int, int, int]]]:
        img = cv2.imread(str(frame))
        if img is None:
            return []
        out = []
        for face in app.get(img):
            x1, y1, x2, y2 = (int(v) for v in face.bbox)
            out.append((face.normed_embedding.tolist(), (x1, y1, x2 - x1, y2 - y1)))
        return out

    return embed


def cosine(a: Embedding, b: Embedding) -> float:
    dot = sum(x * y for x, y in zip(a, b, strict=True))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(x * x for x in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def centroid(embeddings: list[Embedding]) -> Embedding:
    if not embeddings:
        raise ValueError("cannot take centroid of no embeddings")
    dim = len(embeddings[0])
    return [sum(e[i] for e in embeddings) / len(embeddings) for i in range(dim)]


def match(
    embedding: Embedding, gallery: dict[str, Embedding], threshold: float = SIM_THRESHOLD
) -> tuple[str, float] | None:
    """Best gallery identity above threshold, or None. Confidence maps the
    similarity margin above threshold into (0, 1]."""
    best_id, best_sim = None, threshold
    for pid, center in gallery.items():
        sim = cosine(embedding, center)
        if sim > best_sim:
            best_id, best_sim = pid, sim
    if best_id is None:
        return None
    conf = min(1.0, (best_sim - threshold) / (1.0 - threshold) + 0.5)
    return best_id, round(conf, 4)


class FaceRecognizer:
    name = "face"

    def __init__(
        self,
        roster: Roster,
        bank_dir: str | Path,
        embedder: EmbedFn | None = None,
        fps: float = 0.5,
        threshold: float = SIM_THRESHOLD,
    ):
        self.roster = roster
        self.embedder = embedder if embedder is not None else default_embedder()
        self.fps = fps
        self.threshold = threshold
        self.gallery = self._enroll(Path(bank_dir))

    def _enroll(self, bank_dir: Path) -> dict[str, Embedding]:
        """Bank layout: `<bank>/<player-id-with-underscores>/*.jpg` (":" is
        awkward in dirnames, so "player:webb-logan" -> "player_webb-logan").
        Every image contributes its largest detected face."""
        gallery: dict[str, Embedding] = {}
        for player_dir in sorted(p for p in bank_dir.iterdir() if p.is_dir()):
            pid = player_dir.name.replace("_", ":", 1)
            if not self.roster.get(pid):
                continue
            embeddings = []
            for img in sorted(player_dir.iterdir()):
                if img.suffix.lower() not in IMAGE_EXTS:
                    continue
                faces = self.embedder(img)
                if faces:
                    largest = max(faces, key=lambda f: f[1][2] * f[1][3])
                    embeddings.append(largest[0])
            if embeddings:
                gallery[pid] = centroid(embeddings)
        if not gallery:
            raise RecognizerUnavailable(f"no enrollable faces found under {bank_dir}")
        return gallery

    def recognize(self, media_path: Path) -> list[Cue]:
        sightings: list[tuple[float, str, float]] = []
        for t, frame in iter_frames(media_path, fps=self.fps):
            for embedding, _bbox in self.embedder(frame):
                hit = match(embedding, self.gallery, self.threshold)
                if hit:
                    sightings.append((t, hit[0], hit[1]))
        return collapse_sightings(
            sightings, source=self.name, event="appearance", frame_gap=1.0 / self.fps
        )
