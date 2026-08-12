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


# --- a bank with nothing in it is known before the models load (#845) --------
#
# The missing-bank case above already refuses before the embedder is built. A
# bank that EXISTS and holds nothing enrollable is the same class of problem:
# the recognizer cannot run, and loading five ONNX models first buys nothing.


def _recorder(built: list[str]):
    """A stand-in `default_embedder` that records that it was called."""

    def default_embedder():
        built.append("loaded")
        return _embedder()

    return default_embedder


@pytest.mark.parametrize(
    "case",
    ["no player directory", "no roster player", "no image"],
)
def test_a_bank_with_nothing_to_enroll_refuses_before_the_embedder(
    tmp_path, roster, monkeypatch, case
):
    """Each way a bank can hold nothing enrollable, measured on the ONE thing
    that costs seconds: whether the models were loaded."""
    bank = tmp_path / "bank"
    if case == "no player directory":
        bank.mkdir()
    elif case == "no roster player":
        bank = _bank(tmp_path, player_dir="player_not-on-this-roster")
    else:
        (bank / PLAYER_DIR).mkdir(parents=True)
        (bank / PLAYER_DIR / "notes.txt").write_text("no image ever got copied here")

    built: list[str] = []
    monkeypatch.setattr(faces, "default_embedder", _recorder(built))
    with pytest.raises(RecognizerUnavailable):
        FaceRecognizer(roster, bank)
    assert built == [], f"the models were loaded for a bank with {case}"


def test_a_directory_named_like_an_image_is_not_an_image(tmp_path, roster,
                                                         monkeypatch):
    """`nested.jpg` is a legal directory name.

    Counting one as an image would let a bank with nothing to enroll past the
    check, build the embedder, and then hand it a path that was never an image.
    So the check tests the suffix AND the file.
    """
    bank = tmp_path / "bank"
    (bank / PLAYER_DIR / "nested.jpg").mkdir(parents=True)

    built: list[str] = []
    monkeypatch.setattr(faces, "default_embedder", _recorder(built))
    with pytest.raises(RecognizerUnavailable) as caught:
        FaceRecognizer(roster, bank)
    assert "image" in str(caught.value)
    assert built == [], "a directory named .jpg bought a model load"


@pytest.mark.skipif(
    hasattr(os, "geteuid") and os.geteuid() == 0,
    reason="root ignores file permissions, so this cannot be provoked",
)
def test_an_image_that_refuses_to_open_is_not_an_enrollable_image(tmp_path, roster,
                                                                 monkeypatch):
    """`is_file()` says nothing about permissions.

    A bank whose only image cannot be read has nothing to enroll, so it must
    refuse before the models load. Left to the embedder, the default adapter
    returns no face for it, which reports a detector problem for a permission
    one.
    """
    bank = _bank(tmp_path, images=("a.jpg",))
    locked = bank / PLAYER_DIR / "a.jpg"
    locked.chmod(0o000)

    built: list[str] = []
    monkeypatch.setattr(faces, "default_embedder", _recorder(built))
    try:
        with pytest.raises(RecognizerUnavailable) as caught:
            FaceRecognizer(roster, bank)
    finally:
        locked.chmod(0o644)  # or tmp_path cleanup fails
    assert "readable image" in str(caught.value), str(caught.value)
    assert built == [], "an unreadable image bought a model load"


@pytest.mark.skipif(
    hasattr(os, "geteuid") and os.geteuid() == 0,
    reason="root ignores file permissions, so this cannot be provoked",
)
def test_an_unreadable_image_costs_only_its_own_player(tmp_path):
    """Degrade by as little as possible, as an unreadable player directory
    already does: the players who can be enrolled are enrolled."""
    two_players = Roster([
        Player(id=PLAYER_ID, name="Logan Webb", number="62"),
        Player(id="player:daylen-lile", name="Daylen Lile", number="4"),
    ])
    bank = _bank(tmp_path)
    other = bank / "player_daylen-lile"
    other.mkdir()
    locked = other / "a.jpg"
    locked.write_bytes(b"stand-in")
    locked.chmod(0o000)
    try:
        rec = FaceRecognizer(two_players, bank, embedder=_embedder())
    finally:
        locked.chmod(0o644)
    assert list(rec.gallery) == [PLAYER_ID]


def test_each_empty_bank_reason_says_which_one_it_is(tmp_path, roster):
    """"Empty" and "missing" send an operator to different places, so they must
    not collapse into one message. Neither may the two ways a bank that exists
    can still hold nobody to enroll."""
    empty = tmp_path / "empty"
    empty.mkdir()
    stranger = _bank(tmp_path / "stranger", player_dir="player_not-on-this-roster")
    imageless = tmp_path / "imageless"
    (imageless / PLAYER_DIR).mkdir(parents=True)

    def reason(bank: Path) -> str:
        with pytest.raises(RecognizerUnavailable) as caught:
            FaceRecognizer(roster, bank, embedder=_embedder())
        return str(caught.value)

    missing = reason(tmp_path / "no-such-bank")
    assert "not found" in missing

    no_dirs = reason(empty)
    assert "empty" in no_dirs, f"an empty bank does not say so: {no_dirs}"

    off_roster = reason(stranger)
    assert "roster" in off_roster, f"the reason hides the roster: {off_roster}"
    assert "player_not-on-this-roster" in off_roster, off_roster

    no_images = reason(imageless)
    assert "image" in no_images, f"the reason hides the missing images: {no_images}"
    assert PLAYER_DIR in no_images, no_images

    # Four different problems, four different fixes, so four distinct messages.
    assert len({missing, no_dirs, off_roster, no_images}) == 4
    for text in (missing, no_dirs, off_roster, no_images):
        assert "face bank" in text, text


def test_an_empty_bank_is_not_reported_as_a_missing_wheel(tmp_path, roster,
                                                          monkeypatch):
    """The operator-facing end of #845.

    A host without the face extra installed reports "install insightface". A
    host WITH an empty bank must not borrow that message, because installing a
    wheel would not fix an empty directory. The cheap check runs first, so the
    reason the pipeline records is the true one.
    """
    def uninstalled():
        raise RecognizerUnavailable(
            "face recognition needs insightface + opencv "
            "(pip install 'dirstral-annotator[face]')"
        )

    monkeypatch.setattr(faces, "default_embedder", uninstalled)

    empty = tmp_path / "empty-bank"
    empty.mkdir()
    media = tmp_path / "clip.mp4"
    media.write_bytes(b"stand-in; no recognizer reaches frame extraction here")

    pipeline = Pipeline(roster=roster, faces_bank=empty)
    assert pipeline.cues_for(media) == []
    note = " ".join(pipeline.skipped)
    assert "empty" in note, f"an empty bank was reported as something else: {note}"
    assert "insightface" not in note, note


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
