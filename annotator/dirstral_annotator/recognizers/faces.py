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

The bank is an optional backend asset, so it follows the package rule for one
of those: a bank that cannot be enrolled from raises `RecognizerUnavailable` at
construction and the pipeline skips this recognizer with a note. That covers a
bank that is missing, is not a directory or cannot be read
(`_bank_player_dirs`), and a bank that exists and holds nobody to enroll
(`_enrollable_players`). Both are checked before the embedder is built, and each
reason names its own problem, because an empty bank and a missing bank send an
operator to different places.
"""

from __future__ import annotations

import logging
import math
import os
from collections.abc import Callable
from pathlib import Path

from ..model import Cue
from ..roster import Roster
from .base import RecognizerUnavailable, collapse_sightings, iter_frames

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

#: Accepted values. Anything else is rejected rather than defaulted, so a typo
#: cannot quietly select a provider the caller did not ask for.
_PROVIDER_CHOICES = frozenset({"auto", "cpu", "cuda"})


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
    # Strip first, then fall back: a variable set to whitespace is a blank
    # variable, and should mean "unset" exactly as an empty one does.
    want = (requested or "").strip().lower()
    if not want:
        want = (os.environ.get(PROVIDER_ENV) or "").strip().lower() or "auto"
    if want not in _PROVIDER_CHOICES:
        # Silently treating a typo as "auto" is the same class of failure this
        # function exists to remove: "gpu" or "none" would look accepted and run
        # on whatever happened to be available.
        raise RecognizerUnavailable(
            f"unknown face provider {want!r}; expected one of "
            f"{', '.join(sorted(_PROVIDER_CHOICES))} "
            f"(set via {PROVIDER_ENV} or the provider argument)"
        )
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
    """Providers the ONNX sessions actually bound, best effort.

    Prefers the `recognition` model. insightface builds a session per model and
    they can bind differently, so reporting whichever happened to be first could
    claim CUDA while the heaviest model quietly ran on CPU. Recognition is the
    one whose cost dominates, so it is the honest one to report.
    """
    try:
        models = getattr(app, "models", {}) or {}
        sessions = {
            name: getattr(model, "session", None) for name, model in models.items()
        }
        bound = {
            name: list(sess.get_providers())
            for name, sess in sessions.items()
            if sess is not None
        }
        if not bound:
            return []
        if len({tuple(v) for v in bound.values()}) > 1:
            logging.getLogger(__name__).warning(
                "face models bound different execution providers: %s", bound
            )
        for preferred in ("recognition", "detection"):
            if preferred in bound:
                return bound[preferred]
        return next(iter(bound.values()))
    except Exception:  # pragma: no cover - diagnostics must never break a run
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


def _bank_player_dirs(bank_dir: Path) -> list[Path]:
    """The bank's player subdirectories, or an unavailable recognizer.

    A bank that is absent, is not a directory, or cannot be read is a missing
    optional ASSET, the same class of problem as an uninstalled insightface
    wheel. This package has one rule for that class, stated in the pipeline
    docstring: raise `RecognizerUnavailable` at construction, so the pipeline
    records a skip note and the rest of the cascade still runs. Letting the raw
    `OSError` out instead made one absent directory fail the whole annotation
    request: the served backend answered 502 and the eval CLI exited with a
    traceback (#651).

    Only filesystem errors are converted. Anything else is a bug in this package
    and is left to surface as one.
    """
    try:
        return sorted(p for p in bank_dir.iterdir() if p.is_dir())
    except NotADirectoryError as exc:
        raise RecognizerUnavailable(
            f"face bank is not a directory: {bank_dir}"
        ) from exc
    except FileNotFoundError as exc:
        raise RecognizerUnavailable(
            f"face bank not found: {bank_dir} "
            "(expected <bank>/<player-id-with-underscores>/*.jpg)"
        ) from exc
    except OSError as exc:
        # PermissionError and everything else the filesystem can answer with.
        # `strerror` rather than `str(exc)`: the reason belongs in the message,
        # the path is already there, and nothing else about the caller is.
        raise RecognizerUnavailable(
            f"face bank cannot be read: {bank_dir} "
            f"({exc.strerror or type(exc).__name__})"
        ) from exc


def _enrollable_players(
    bank_dir: Path, player_dirs: list[Path], roster: Roster
) -> list[tuple[str, list[Path]]]:
    """Each roster player in the bank that has an image, or an unavailable
    recognizer. `(player id, images)` in bank order.

    A bank that EXISTS and holds nobody to enroll is the same missing asset as a
    bank that is not there, so it takes the same route: `RecognizerUnavailable`
    at construction, before the embedder is built. The check is a directory
    listing per player, and the embedder it stands in front of loads and prepares
    five ONNX models, measured at 0.5 s (#845).

    The reason names WHICH problem it is. An empty bank, a bank whose directories
    are named for players nobody rostered, a rostered player whose images never
    got copied, and images that refuse to open are different mistakes with
    different fixes, so they must not collapse into one message.

    One unreadable player directory, or one unreadable image, still costs only
    that player: it is skipped with a warning that says who was lost, and the
    rest of the bank still enrolls. A bank that leaves nobody after that ends as
    unavailability here.
    """
    enrollable: list[tuple[str, list[Path]]] = []
    off_roster: list[str] = []
    imageless: list[str] = []
    unreadable: list[str] = []
    locked_images: list[str] = []
    for player_dir in player_dirs:
        pid = player_dir.name.replace("_", ":", 1)
        if not roster.get(pid):
            off_roster.append(player_dir.name)
            continue
        try:
            entries = sorted(player_dir.iterdir())
        except OSError as exc:
            logging.getLogger(__name__).warning(
                "face bank: skipping %s (%s)",
                player_dir.name, exc.strerror or type(exc).__name__,
            )
            unreadable.append(player_dir.name)
            continue
        # `is_file()` as well as the suffix: a DIRECTORY called `nested.jpg`
        # would otherwise count as an image, so a bank with nothing to enroll
        # would pass this check, build the embedder, and hand it a path that was
        # never an image. A broken symlink is excluded for the same reason.
        named = [p for p in entries if p.is_file() and p.suffix.lower() in IMAGE_EXTS]
        if not named:
            imageless.append(player_dir.name)
            continue
        # An image the process cannot open is not an image it can enroll from.
        # `is_file()` says nothing about permissions, so each candidate is opened
        # here: the alternative is that the models load and the adapter then
        # returns no face, which reports a detector problem for a permission one.
        # One open per image, against about a second per image to embed.
        images = []
        for image in named:
            try:
                with image.open("rb"):
                    pass
            except OSError as exc:
                logging.getLogger(__name__).warning(
                    "face bank: skipping %s/%s (%s)",
                    player_dir.name, image.name,
                    exc.strerror or type(exc).__name__,
                )
                continue
            images.append(image)
        if not images:
            locked_images.append(player_dir.name)
            continue
        enrollable.append((pid, images))
    if enrollable:
        return enrollable

    exts = ", ".join(sorted(IMAGE_EXTS))
    if not player_dirs:
        raise RecognizerUnavailable(
            f"face bank is empty: {bank_dir} holds no player directory "
            "(expected <bank>/<player-id-with-underscores>/*.jpg)"
        )
    # Most actionable first. A directory named for a rostered player is one an
    # operator meant to fill, so say it is unfilled rather than counting the
    # strangers beside it.
    if imageless:
        raise RecognizerUnavailable(
            "face bank has no image for any rostered player: "
            f"{', '.join(imageless)} under {bank_dir} hold no {exts} file"
        )
    if locked_images:
        raise RecognizerUnavailable(
            "face bank has no readable image for any rostered player: "
            f"every image under {', '.join(locked_images)} in {bank_dir} "
            "refused to open (check the file permissions)"
        )
    if unreadable:
        raise RecognizerUnavailable(
            "face bank has no readable player directory: "
            f"{', '.join(unreadable)} under {bank_dir} could not be read"
        )
    # Every remaining directory is off the roster, or `enrollable` would have
    # won: the five buckets above cover every player directory.
    raise RecognizerUnavailable(
        f"face bank has no player on this roster: {', '.join(off_roster)} "
        f"under {bank_dir} match no roster id "
        "(expected <bank>/<player-id-with-underscores>, "
        "e.g. player_webb-logan for player:webb-logan)"
    )


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
        bank = Path(bank_dir)
        # The bank is read BEFORE the embedder is built, and read far enough to
        # know there is somebody to enroll. Building the default embedder loads
        # and prepares five ONNX models, and paying that only to then find an
        # empty bank is waste with no upside. Both failures are
        # RecognizerUnavailable, so the cheap one goes first (#651, #845).
        candidates = _enrollable_players(bank, _bank_player_dirs(bank), roster)
        self.embedder = embedder if embedder is not None else default_embedder()
        self.fps = fps
        self.threshold = threshold
        self.gallery = self._enroll(bank, candidates)

    def _enroll(
        self, bank_dir: Path, candidates: list[tuple[str, list[Path]]]
    ) -> dict[str, Embedding]:
        """Bank layout: `<bank>/<player-id-with-underscores>/*.jpg` (":" is
        awkward in dirnames, so "player:webb-logan" -> "player_webb-logan").
        Every image contributes its largest detected face.

        `candidates` is what `_enrollable_players` already found: the rostered
        players that have images. So the only failure left here is the one that
        needs the embedder to know it, which is an image the detector finds no
        face in.
        """
        gallery: dict[str, Embedding] = {}
        images_read = 0
        for pid, images in candidates:
            embeddings = []
            for img in images:
                images_read += 1
                faces = self.embedder(img)
                if faces:
                    largest = max(faces, key=lambda f: f[1][2] * f[1][3])
                    embeddings.append(largest[0])
            if embeddings:
                gallery[pid] = centroid(embeddings)
        if not gallery:
            raise RecognizerUnavailable(
                "face bank has no detectable face: the embedder found none in "
                f"the {images_read} image(s) under {bank_dir} "
                "(bank images should show one clear, front-facing face)"
            )
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
