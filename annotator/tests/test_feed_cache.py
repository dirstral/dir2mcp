"""One read of a game feed per binding, not one per request.

`Pipeline.cues_for` builds `PlayByPlayRecognizer` per request, and building it
calls `GameConfig.events()`. That re-read the saved GUMBO JSON from disk, or
re-fetched it from MLB statsapi when the binding uses `game_pk`, on every POST to
/recognize (#844).

Measured on a real 0.901 MB GUMBO feed, 344 pitches, this host:

    events() saved feed   median   15.33 ms per request
    events() game_pk      median 2892.32 ms per request (min 2211, max 5953)

The disk number is noise beside the 46 s that PR #839 removed, and beside the
seconds of inference a real request spends. The network number is not: it adds
seconds to every request and it puts a third party service in the request path.
So the memo below is about the fetch, and the disk read rides along because the
two paths are one line of code.

The memo lives on the `GameConfig` the operator's games.json created, so it needs
no eviction policy: what stays resident is bounded by the bindings that are
configured, not by the requests that arrive (69.5 KiB of parsed events for that
344 pitch game; the 2.77 MiB raw payload is parsed and dropped).

Backend-free: no network, no ffmpeg, no CV backend. The feed loader is stubbed,
which is also how the read count is counted.
"""

from __future__ import annotations

import json
import threading
import time
from pathlib import Path

import pytest

from dirstral_annotator.eval import ground_truth
from dirstral_annotator.model import Player
from dirstral_annotator.pipeline import GameConfig, Pipeline
from dirstral_annotator.roster import Roster

FIXTURE = Path(__file__).parent / "fixtures" / "gumbo_min.json"
WEBB = 657277

#: Read at import time, so no test's own read count includes the anchor it needs.
FIRST_PITCH_EPOCH = ground_truth.parse_pitches(
    ground_truth.load_game(FIXTURE)
)[0].epoch_s


@pytest.fixture
def roster() -> Roster:
    return Roster(
        [Player(id="player:webb-logan", name="Logan Webb", number="62")],
        {WEBB: "player:webb-logan"},
    )


@pytest.fixture
def media(tmp_path) -> Path:
    path = tmp_path / "game7.mp4"
    path.write_bytes(b"stand-in; no recognizer here reads a frame")
    return path


def _counted(monkeypatch, name: str, payload=None, delay: float = 0.0):
    """Replace `ground_truth.<name>` with a counting stub. Returns the log."""
    reads: list[str] = []
    feed = payload if payload is not None else ground_truth.load_game(FIXTURE)

    def stub(*args, **kwargs):
        reads.append(name)
        if delay:
            time.sleep(delay)
        return feed

    monkeypatch.setattr(ground_truth, name, stub)
    return reads


def _pipeline(roster: Roster, media: Path, game: GameConfig) -> Pipeline:
    return Pipeline(roster=roster, games={media.name: game})


def _anchored(**binding) -> GameConfig:
    """A binding anchored on the fixture's first pitch, so cues land on the
    video timeline. It reads no feed of its own."""
    return GameConfig.parse({"anchors": [f"{FIRST_PITCH_EPOCH}=60.0"], **binding})


# --- the headline contract --------------------------------------------------

def test_two_requests_read_a_saved_feed_once(monkeypatch, roster, media):
    reads = _counted(monkeypatch, "load_game")
    pipeline = _pipeline(roster, media, _anchored(feed=str(FIXTURE)))

    first = pipeline.cues_for(media)
    second = pipeline.cues_for(media)

    assert len(reads) == 1, f"the saved feed was read per request: {len(reads)}"
    assert first, "the fixture must produce cues, or this proves nothing"
    assert first == second, "the memo stopped answering"


def test_two_requests_fetch_a_game_pk_once(monkeypatch, roster, media):
    """The path the measurement condemns: 2.9 s median per fetch, from a service
    the recognizer does not need to be live."""
    fetches = _counted(monkeypatch, "fetch_game")
    pipeline = _pipeline(roster, media, _anchored(game_pk=823215))

    first = pipeline.cues_for(media)
    second = pipeline.cues_for(media)

    assert len(fetches) == 1, f"statsapi was called per request: {len(fetches)}"
    assert first and first == second


def test_concurrent_first_requests_read_the_feed_once(monkeypatch, roster, media):
    """Two requests that arrive together must not both pay the read.

    Same rule as the recognizer cache (#650): the read holds a lock, so the
    second request waits for the first result instead of duplicating a fetch that
    is measured in seconds.
    """
    reads = _counted(monkeypatch, "load_game", delay=0.05)
    pipeline = _pipeline(roster, media, _anchored(feed=str(FIXTURE)))

    start = threading.Barrier(2)
    results: list[list] = []
    guard = threading.Lock()

    def request():
        start.wait(timeout=5)
        cues = pipeline.cues_for(media)
        with guard:
            results.append(cues)

    threads = [threading.Thread(target=request) for _ in range(2)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=10)

    assert len(reads) == 1, f"concurrent first requests both read the feed: {reads}"
    assert len(results) == 2 and results[0] == results[1]


# --- the failure path ------------------------------------------------------

def test_a_feed_that_stops_reading_cannot_break_a_later_request(
    monkeypatch, roster, media
):
    """The question #844 asks: a feed that read at the first request and does not
    read at the tenth.

    With the memo the tenth request does not read at all, so a service outage or
    a deleted file after the first request cannot take the labels away. Before
    the memo the tenth request raised, and `cues_for` calls `events()` outside
    `try_recognizer`, so it took the whole cascade with it: 502 from the served
    backend.
    """
    calls: list[int] = []
    payload = ground_truth.load_game(FIXTURE)

    def once_then_never(*args, **kwargs):
        calls.append(1)
        if len(calls) > 1:
            raise OSError("statsapi is down / the saved feed was deleted")
        return payload

    monkeypatch.setattr(ground_truth, "fetch_game", once_then_never)
    pipeline = _pipeline(roster, media, _anchored(game_pk=823215))

    first = pipeline.cues_for(media)
    assert first
    for _ in range(9):
        assert pipeline.cues_for(media) == first
    assert len(calls) == 1


def test_a_first_read_failure_is_not_remembered(monkeypatch, roster, media):
    """Only a successful read is remembered.

    Remembering a failure would turn one bad moment into a process that never
    recognizes play-by-play again, which is the opposite of what the memo is for.
    A failure that is not remembered keeps today's behaviour: the request fails,
    and the next request tries again.
    """
    calls: list[int] = []
    payload = ground_truth.load_game(FIXTURE)

    def broken_then_fixed(*args, **kwargs):
        calls.append(1)
        if len(calls) == 1:
            raise FileNotFoundError("the feed had not been copied yet")
        return payload

    monkeypatch.setattr(ground_truth, "load_game", broken_then_fixed)
    pipeline = _pipeline(roster, media, _anchored(feed=str(FIXTURE)))

    with pytest.raises(FileNotFoundError):
        pipeline.cues_for(media)
    assert pipeline.cues_for(media), "the failure was remembered as no events"
    assert len(calls) == 2


# --- the memo may not outlive the binding it was built for ------------------

def test_a_repointed_feed_is_not_hidden_by_the_memo(tmp_path, roster, media):
    """The same rule the recognizer cache follows: the memo is keyed on what
    decides its contents, so an operator who repoints the binding gets the new
    feed rather than the old parse."""
    original = ground_truth.load_game(FIXTURE)
    later = json.loads(json.dumps(original))
    # One pitch fewer, so the two feeds are distinguishable from the cues alone.
    later["liveData"]["plays"]["allPlays"] = \
        later["liveData"]["plays"]["allPlays"][:1]
    second_feed = tmp_path / "gumbo_second.json"
    second_feed.write_text(json.dumps(later), encoding="utf-8")

    game = _anchored(feed=str(FIXTURE))
    pipeline = _pipeline(roster, media, game)

    before = pipeline.cues_for(media)
    game.feed = str(second_feed)
    after = pipeline.cues_for(media)

    assert before and after
    assert len(after) < len(before), "the repointed feed reused the old parse"


def test_a_saved_feed_edited_on_disk_is_read_once_per_run(monkeypatch, roster,
                                                          media):
    """The stated invalidation rule, pinned so it cannot drift by accident.

    A binding that still points at the same path keeps the parse it made. Every
    request in a run therefore answers from one label set, which is the
    consistency PR #839 chose for the face bank: before it, a feed edited during
    a run reached whichever request happened to re-read it, and nothing recorded
    which. An operator who wants the new file reloads the bindings.
    """
    reads = _counted(monkeypatch, "load_game")
    game = _anchored(feed=str(FIXTURE))
    pipeline = _pipeline(roster, media, game)

    pipeline.cues_for(media)
    pipeline.cues_for(media)
    assert len(reads) == 1

    # A fresh binding is a fresh read, which is what reloading games.json gives.
    pipeline.games = {media.name: _anchored(feed=str(FIXTURE))}
    pipeline.cues_for(media)
    assert len(reads) == 2


def test_the_memo_is_not_part_of_the_configuration():
    """`GameConfig` is a dataclass an operator's file produced. Reading its feed
    must not change what it compares or prints as, or a caller that logs a
    binding would dump a whole game into a log line.
    """
    a = _anchored(feed=str(FIXTURE))
    b = _anchored(feed=str(FIXTURE))
    assert a == b
    a.events()
    assert a == b, "reading the feed changed the configuration's identity"
    assert "PitchEvent" not in repr(a)
