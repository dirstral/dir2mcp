"""A face bank that is not there must degrade the cascade, not abort it.

The annotator has one rule for an optional backend, stated in the pipeline and
`base` module docstrings: a recognizer whose backend is missing raises
`RecognizerUnavailable` at construction, the pipeline records a skip note, and
the rest of the cascade still runs. A face bank is a backend asset exactly like
a model wheel, so a missing or unreadable one follows that same rule.

It did not. `_enroll` called `bank_dir.iterdir()` with nothing around it, so a
path that was not there raised `FileNotFoundError`, which `try_recognizer` does
not convert. One missing optional asset took down the whole annotation request:
the served backend answered 502 and the eval CLI exited with a traceback (#651).

Backend-free: the embedding callable is injected, so nothing here needs
insightface, onnxruntime or a model file.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from dirstral_annotator.model import Cue, Player
from dirstral_annotator.pipeline import Pipeline
from dirstral_annotator.recognizers import faces, scorebug
from dirstral_annotator.recognizers.base import RecognizerUnavailable
from dirstral_annotator.recognizers.faces import FaceRecognizer
from dirstral_annotator.roster import Roster

PLAYER_ID = "player:webb-logan"
#: The bank's directory name for that id: ":" is awkward in a dirname, so the
#: layout uses an underscore.
PLAYER_DIR = "player_webb-logan"

#: Two 4-dimensional unit vectors, close enough to average into a usable
#: centroid. The numbers only have to be embeddings; nothing here scores them.
FACE_A = [1.0, 0.0, 0.0, 0.0]
FACE_B = [0.9, 0.1, 0.0, 0.0]

BBOX = (0, 0, 40, 40)


@pytest.fixture
def roster() -> Roster:
    return Roster([Player(id=PLAYER_ID, name="Logan Webb", number="62")])


def _embedder(per_image=None):
    """An embedding callable that answers from a per-filename table."""
    table = per_image or {}

    def embed(image: Path):
        return [(table.get(image.name, FACE_A), BBOX)]

    return embed


def _bank(tmp_path: Path, *, player_dir: str = PLAYER_DIR, images=("a.jpg", "b.jpg")):
    bank = tmp_path / "bank"
    (bank / player_dir).mkdir(parents=True)
    for name in images:
        (bank / player_dir / name).write_bytes(b"stand-in; embedding is injected")
    return bank


# --- the happy path, so the failures below mean something -------------------

def test_a_readable_bank_still_enrolls(tmp_path, roster):
    bank = _bank(tmp_path)
    rec = FaceRecognizer(roster, bank, embedder=_embedder({"b.jpg": FACE_B}))
    assert list(rec.gallery) == [PLAYER_ID]
    assert rec.gallery[PLAYER_ID] == pytest.approx(
        [(a + b) / 2 for a, b in zip(FACE_A, FACE_B)]
    )


# --- the missing asset cases ------------------------------------------------

def test_a_missing_bank_is_unavailable_not_a_crash(tmp_path, roster):
    missing = tmp_path / "no-such-bank"
    with pytest.raises(RecognizerUnavailable) as caught:
        FaceRecognizer(roster, missing, embedder=_embedder())
    # The operator has to be able to act on this: it must name the asset and the
    # path they configured.
    assert "face bank" in str(caught.value)
    assert str(missing) in str(caught.value)


def test_a_bank_that_is_a_file_is_unavailable(tmp_path, roster):
    not_a_dir = tmp_path / "bank.tar"
    not_a_dir.write_bytes(b"an archive somebody forgot to unpack")
    with pytest.raises(RecognizerUnavailable) as caught:
        FaceRecognizer(roster, not_a_dir, embedder=_embedder())
    assert "directory" in str(caught.value)


@pytest.mark.skipif(
    hasattr(os, "geteuid") and os.geteuid() == 0,
    reason="root ignores directory permissions, so this cannot be provoked",
)
def test_an_unreadable_bank_is_unavailable(tmp_path, roster):
    bank = _bank(tmp_path)
    bank.chmod(0o000)
    try:
        with pytest.raises(RecognizerUnavailable) as caught:
            FaceRecognizer(roster, bank, embedder=_embedder())
    finally:
        bank.chmod(0o755)  # or tmp_path cleanup fails
    assert "face bank" in str(caught.value)


def test_a_bank_with_nothing_enrollable_is_unavailable(tmp_path, roster):
    """An empty bank, and a bank whose only player is not on the roster, are
    both "nothing to recognize with" rather than a fatal error."""
    empty = tmp_path / "empty"
    empty.mkdir()
    with pytest.raises(RecognizerUnavailable):
        FaceRecognizer(roster, empty, embedder=_embedder())

    stranger = _bank(tmp_path / "other", player_dir="player_not-on-this-roster")
    with pytest.raises(RecognizerUnavailable):
        FaceRecognizer(roster, stranger, embedder=_embedder())


@pytest.mark.skipif(
    hasattr(os, "geteuid") and os.geteuid() == 0,
    reason="root ignores directory permissions, so this cannot be provoked",
)
def test_an_unreadable_player_directory_costs_only_that_player(tmp_path):
    """One player's images being unreadable is not the bank being unreadable.

    Degrading means degrading by as little as possible: the players who can be
    enrolled are enrolled, and the recognizer runs.
    """
    two_players = Roster([
        Player(id=PLAYER_ID, name="Logan Webb", number="62"),
        Player(id="player:daylen-lile", name="Daylen Lile", number="4"),
    ])
    bank = _bank(tmp_path)
    locked = bank / "player_daylen-lile"
    locked.mkdir()
    (locked / "a.jpg").write_bytes(b"stand-in")
    locked.chmod(0o000)
    try:
        rec = FaceRecognizer(two_players, bank, embedder=_embedder())
    finally:
        locked.chmod(0o755)
    assert list(rec.gallery) == [PLAYER_ID]


# --- what must NOT be converted --------------------------------------------

def test_an_embedder_bug_still_raises(tmp_path, roster):
    """Only expected asset and configuration failures become unavailability.

    A callable that is simply wrong is a programming error, and hiding it behind
    a skip note would turn a bug into a silently missing recognizer.
    """
    bank = _bank(tmp_path)

    def broken(image: Path):
        raise ZeroDivisionError("a bug inside the embedding adapter")

    with pytest.raises(ZeroDivisionError):
        FaceRecognizer(roster, bank, embedder=broken)


# --- the cascade keeps going -----------------------------------------------

def test_the_pipeline_skips_a_missing_bank_and_keeps_running(tmp_path, roster,
                                                             monkeypatch):
    """The end the issue is actually about: one absent optional asset must cost
    the face cues and nothing else.

    A second recognizer stands in for the rest of the cascade. Its cue has to
    reach the caller, alongside a skip note that names what was lost, which is
    what "partial annotations" means here.
    """
    monkeypatch.setattr(faces, "default_embedder", _embedder)

    survivor = Cue(source="scorebug", start_s=1.0, end_s=2.0, event="at_bat",
                   entity_ids=(PLAYER_ID,), confidence=0.9)

    class _Scorebug:
        name = "scorebug"

        def __init__(self, *args, **kwargs):
            pass

        def recognize(self, media_path: Path):
            return [survivor]

    monkeypatch.setattr(scorebug, "ScorebugRecognizer", _Scorebug)

    media = tmp_path / "clip.mp4"
    media.write_bytes(b"stand-in; no recognizer reaches frame extraction here")

    pipeline = Pipeline(roster=roster, scorebug=True,
                        faces_bank=tmp_path / "no-such-bank")
    cues = pipeline.cues_for(media)

    assert cues == [survivor], "the missing bank took the rest of the cascade too"
    assert pipeline.skipped, "a missing face bank produced no skip note"
    note = " ".join(pipeline.skipped)
    assert "face bank" in note, f"the skip note does not name the asset: {note}"


def test_the_bank_is_checked_before_the_embedder_is_built(tmp_path, roster,
                                                          monkeypatch):
    """Building the default embedder loads and prepares the ONNX models, which
    is measured in seconds. A bank that is not there is known before any of that
    is worth paying for."""
    built: list[int] = []

    def recording_default_embedder():
        built.append(1)
        return _embedder()

    monkeypatch.setattr(faces, "default_embedder", recording_default_embedder)
    with pytest.raises(RecognizerUnavailable):
        FaceRecognizer(roster, tmp_path / "no-such-bank")
    assert built == [], "the models were loaded for a bank that does not exist"
