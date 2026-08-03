"""Frame-extraction plumbing: caching, cleanup and timeout derivation.

These pin the four review findings on the caching change. Each one describes a
failure that costs disk or aborts a legitimate run, and none had coverage.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from dirstral_annotator.recognizers import base


@pytest.fixture(autouse=True)
def _clear_cache():
    base._cleanup_frame_dirs()
    yield
    base._cleanup_frame_dirs()


def _fake_run(frames: int):
    """Stand in for ffmpeg, writing `frames` jpegs into the -vf output dir."""
    def run(cmd, **kw):
        out = Path(cmd[-1])
        for i in range(1, frames + 1):
            (out.parent / f"frame-{i:08d}.jpg").write_bytes(b"\xff\xd8\xff")
        return subprocess.CompletedProcess(cmd, 0, b"", b"")
    return run


def test_extraction_is_shared_across_recognizers(tmp_path, monkeypatch):
    """The cascade must decode once, not once per recognizer (the bug that made
    a 3h broadcast cost three hour-long passes)."""
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)
    calls = []

    def run(cmd, **kw):
        calls.append(cmd)
        return _fake_run(3)(cmd, **kw)

    monkeypatch.setattr(base.subprocess, "run", run)
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)

    for _ in range(3):  # scorebug, jersey, faces
        assert len(list(base.iter_frames(media, fps=0.5))) == 3
    assert len(calls) == 1, f"expected one extraction, got {len(calls)}"


def test_failed_extraction_leaves_no_directory(tmp_path, monkeypatch):
    """A failure must not strand a partially written JPEG set until exit."""
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)
    created: list[Path] = []
    real_mkdtemp = base.tempfile.mkdtemp

    def mkdtemp(**kw):
        d = real_mkdtemp(**kw)
        created.append(Path(d))
        return d

    monkeypatch.setattr(base.tempfile, "mkdtemp", mkdtemp)
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)
    monkeypatch.setattr(base.subprocess, "run", lambda cmd, **kw: (_ for _ in ()).throw(
        subprocess.CalledProcessError(1, cmd, b"", b"boom")))

    with pytest.raises(RuntimeError):
        list(base.iter_frames(media, fps=0.5))
    assert created, "extraction should have created a temp dir"
    assert not created[0].exists(), "temp dir survived a failed extraction"
    assert not base._FRAME_CACHE, "a failed extraction must not be cached"


def test_cache_is_bounded(tmp_path, monkeypatch):
    """`serve` handles many media over its lifetime; an unbounded memo would
    keep every JPEG set until the process exits."""
    monkeypatch.setattr(base.subprocess, "run", _fake_run(1))
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)
    dirs = []
    for i in range(base._FRAME_CACHE_MAX + 2):
        m = tmp_path / f"m{i}.mp4"
        m.write_bytes(b"x" * (32 + i))
        list(base.iter_frames(m, fps=0.5))
        dirs.append(list(base._FRAME_CACHE.values())[-1])
    assert len(base._FRAME_CACHE) == base._FRAME_CACHE_MAX
    assert not dirs[0].exists(), "evicted extraction was not removed from disk"


def test_probe_duration_handles_null(tmp_path, monkeypatch):
    """ffprobe reports "duration": null for some containers; float(None) would
    abort an extraction that should just fall back to no limit."""
    monkeypatch.setattr(base.subprocess, "run", lambda *a, **k: subprocess.CompletedProcess(
        a[0], 0, b'{"format": {"duration": null}}', b""))
    assert base.probe_duration_s(tmp_path / "x.mp4") is None


def test_timeout_scales_with_duration(tmp_path, monkeypatch):
    """A fixed ceiling cannot serve both a clip and a 3h broadcast."""
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 12236.0)
    assert base._extract_timeout_s(tmp_path / "x.mp4", "ffprobe") == pytest.approx(48944.0)
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 5.0)
    assert base._extract_timeout_s(tmp_path / "x.mp4", "ffprobe") == 600.0
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: None)
    assert base._extract_timeout_s(tmp_path / "x.mp4", "ffprobe") == float("inf")


def test_in_use_extraction_is_not_evicted(tmp_path, monkeypatch):
    """serve runs under ThreadingHTTPServer: one request must not rmtree the
    frames another request is mid-iteration on."""
    monkeypatch.setattr(base.subprocess, "run", _fake_run(3))
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)

    held = tmp_path / "held.mp4"
    held.write_bytes(b"x" * 32)
    it = base.iter_frames(held, fps=0.5)
    first = next(it)                      # iterator now holds this extraction
    held_dir = next(iter(base._FRAME_CACHE.values()))

    # Push well past the cap from "other requests".
    for i in range(base._FRAME_CACHE_MAX + 3):
        m = tmp_path / f"other{i}.mp4"
        m.write_bytes(b"x" * (64 + i))
        list(base.iter_frames(m, fps=0.5))

    assert held_dir.exists(), "an in-use extraction was evicted from disk"
    assert first[1].exists(), "the frame being iterated was deleted"
    rest = list(it)                        # must still be readable
    assert len(rest) == 2
    it.close()


def test_concurrent_callers_extract_once(tmp_path, monkeypatch):
    """Two threads asking for the same media must not both decode it."""
    import threading as _t

    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)
    calls = []
    started = _t.Event()

    def slow_run(cmd, **kw):
        calls.append(cmd)
        started.set()
        _t.Event().wait(0.3)          # hold the extraction open
        return _fake_run(2)(cmd, **kw)

    monkeypatch.setattr(base.subprocess, "run", slow_run)
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)

    out = []
    def worker():
        out.append(len(list(base.iter_frames(media, fps=0.5))))

    t1 = _t.Thread(target=worker); t1.start()
    started.wait(2)
    t2 = _t.Thread(target=worker); t2.start()
    t1.join(10); t2.join(10)

    assert out == [2, 2], f"both callers should see all frames, got {out}"
    assert len(calls) == 1, f"expected one extraction, got {len(calls)}"


def test_recognizer_crops_are_not_ingested_as_frames(tmp_path, monkeypatch):
    """Recognizers write siblings into the frame directory: JerseyRecognizer
    saves crops as `frame-<n>-p<x>x<y>.jpg`. Now that the directory is shared
    across the cascade, a loose glob would hand one recognizer's crops to the
    next as sampled frames, with timestamps inflated by the extra entries.
    """
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)
    monkeypatch.setattr(base.subprocess, "run", _fake_run(4))
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)

    first = list(base.iter_frames(media, fps=0.5))
    assert len(first) == 4
    assert first[-1][0] == 6.0, "4 frames at 0.5 fps end at t=6.0"

    # Simulate the jersey recognizer saving crops beside the frames it read.
    frame_dir = next(iter(base._FRAME_CACHE.values()))
    for i, (_, f) in enumerate(first):
        (frame_dir / f"{f.stem}-p{100 + i}x{200 + i}.jpg").write_bytes(b"\xff\xd8\xff")

    second = list(base.iter_frames(media, fps=0.5))
    assert len(second) == 4, f"crops leaked into the frame list: {len(second)} entries"
    assert second[-1][0] == 6.0, "timestamps inflated by ingesting crops"
    assert all("-p" not in p.name for _, p in second)


def test_mkdtemp_failure_does_not_strand_waiters(tmp_path, monkeypatch):
    """The in-flight marker is set before the temp dir exists. A mkdtemp failure
    that left it set would block every later caller for that media forever."""
    import threading as _t
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)

    monkeypatch.setattr(base.tempfile, "mkdtemp",
                        lambda **kw: (_ for _ in ()).throw(OSError("no space left on device")))
    with pytest.raises(OSError):
        list(base.iter_frames(media, fps=0.5))
    assert not base._FRAME_INFLIGHT, "in-flight marker survived the failure"

    # A later caller must proceed rather than block.
    monkeypatch.undo()
    monkeypatch.setattr(base.subprocess, "run", _fake_run(2))
    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)
    done = _t.Event()
    out = []
    def worker():
        out.append(len(list(base.iter_frames(media, fps=0.5))))
        done.set()
    _t.Thread(target=worker, daemon=True).start()
    assert done.wait(10), "a later caller blocked forever after a mkdtemp failure"
    assert out == [2]


def test_fresh_extraction_is_claimed_before_the_lock_is_released(tmp_path, monkeypatch):
    """A just-extracted entry sits in the cache before its iterator claims it.
    If the claim is not made in the same critical section, another thread
    trimming to the cap can rmtree the directory the caller is about to read.
    """
    import threading as _t

    monkeypatch.setattr(base, "probe_duration_s", lambda *a, **k: 10.0)
    evicting = _t.Event()

    def run_then_let_others_evict(cmd, **kw):
        out = _fake_run(2)(cmd, **kw)
        evicting.set()          # other threads pile in while we are mid-extract
        return out

    monkeypatch.setattr(base.subprocess, "run", run_then_let_others_evict)

    target = tmp_path / "target.mp4"
    target.write_bytes(b"x" * 32)

    result = {}
    def reader():
        frames = list(base.iter_frames(target, fps=0.5))
        result["ok"] = len(frames) == 2 and all(p.exists() for _, p in frames)

    t = _t.Thread(target=reader); t.start()
    evicting.wait(5)
    for i in range(base._FRAME_CACHE_MAX + 3):   # push hard on the cap
        m = tmp_path / f"filler{i}.mp4"
        m.write_bytes(b"x" * (64 + i))
        list(base.iter_frames(m, fps=0.5))
    t.join(10)
    assert result.get("ok"), "a freshly extracted directory was evicted before its reader claimed it"


def test_ffprobe_interrupt_does_not_strand_waiters(tmp_path, monkeypatch):
    """_extract_timeout_s shells out to ffprobe. An interrupt there must clean
    up like any other failure, or the temp dir leaks and later callers for this
    media deadlock on the in-flight marker."""
    media = tmp_path / "game.mp4"
    media.write_bytes(b"x" * 32)
    created = []
    real_mkdtemp = base.tempfile.mkdtemp
    monkeypatch.setattr(base.tempfile, "mkdtemp",
                        lambda **kw: created.append(real_mkdtemp(**kw)) or created[-1])
    monkeypatch.setattr(base, "_extract_timeout_s",
                        lambda *a, **k: (_ for _ in ()).throw(KeyboardInterrupt()))

    with pytest.raises(KeyboardInterrupt):
        list(base.iter_frames(media, fps=0.5))

    assert not base._FRAME_INFLIGHT, "in-flight marker survived an interrupt"
    assert not Path(created[0]).exists(), "temp dir survived an interrupt"
