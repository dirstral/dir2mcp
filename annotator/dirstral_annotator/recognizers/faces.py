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

import math
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


def default_embedder() -> EmbedFn:
    try:
        import cv2  # type: ignore
        from insightface.app import FaceAnalysis  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "face recognition needs insightface + opencv "
            "(pip install 'dirstral-annotator[face]')"
        ) from exc

    app = FaceAnalysis(name="buffalo_l")
    app.prepare(ctx_id=-1)  # CPU; pilot-scale sampling rates don't need GPU

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
