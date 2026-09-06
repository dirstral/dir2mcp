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
import unicodedata
from collections import OrderedDict
from collections.abc import Callable, Iterator
from dataclasses import dataclass

#: Bytes kept of a scrubbed error message. Same order of magnitude as dir2mcp's
#: store.SanitizeReason cap: enough for a diagnostic, too little for a payload.
SCRUBBED_ERROR_MAX_CHARS = 512

# What an upstream exception message can carry that a log line must not: a
# credential named in a URL or header ("api_key=...", "Authorization: Bearer
# ..."), a URL query string, or a long opaque token. The captioner and prober
# are injected callables and may be HTTP clients whose exceptions interpolate
# the failing request (#945 review, CWE-209), so the message is scrubbed
# BEFORE it reaches a skip reason, a log line or a response body.
_CREDENTIAL_KV = re.compile(
    # key, separator, then the WHOLE value including an auth-scheme word:
    # "Authorization: Bearer sk-..." must lose the token, not just "Bearer".
    # (The first draft consumed one \S+ and redacted the scheme word alone; a
    # sentinel test caught the token surviving.)
    r"(?i)\b(api[_-]?key|access[_-]?key|secret|token|password|passwd|authorization)"
    r"\s*[=:]\s*(?:(?:bearer|basic|token)\s+)?\S+"
)
# A bare scheme + credential with no key name in front ("Bearer eyJ...").
_BARE_BEARER = re.compile(r"(?i)\b(bearer|basic)\s+\S+")
_URL_QUERY = re.compile(r"(https?://[^\s?#]+)\?[^\s]*")
# Well-known secret prefixes are redacted at ANY length: real keys are often
# shorter than the opaque-token floor below ("sk-" keys can be 20-30 chars).
_PREFIXED_KEY = re.compile(r"\b(?:sk|pk|rk|tk|hf|ghp|gho|ghs|glpat|xox[abp]|AKIA)[-_][A-Za-z0-9_\-]{8,}")
_LONG_OPAQUE_TOKEN = re.compile(r"\b[A-Za-z0-9_\-]{32,}\b")


def scrub_error(exc: BaseException) -> str:
    """One safe line naming the exception type and a scrubbed message.

    Mirrors dir2mcp's store.SanitizeReason (printable characters only,
    whitespace collapsed, length capped) and adds credential redaction, because
    the annotator's backends are injected callables that may be HTTP clients.
    The TYPE is always kept verbatim: it is the part of an exception that is
    diagnostic and never sensitive.
    """
    msg = str(exc)
    msg = _CREDENTIAL_KV.sub(lambda m: m.group(1) + "=<redacted>", msg)
    msg = _BARE_BEARER.sub(lambda m: m.group(1) + " <redacted>", msg)
    msg = _URL_QUERY.sub(lambda m: m.group(1) + "?<redacted>", msg)
    msg = _PREFIXED_KEY.sub("<redacted>", msg)
    msg = _LONG_OPAQUE_TOKEN.sub("<redacted>", msg)
    msg = "".join(ch if ch.isprintable() else " " for ch in msg)
    msg = " ".join(msg.split())
    if len(msg) > SCRUBBED_ERROR_MAX_CHARS:
        msg = msg[: SCRUBBED_ERROR_MAX_CHARS - 1] + "\u2026"
    return f"{type(exc).__name__}: {msg}" if msg else type(exc).__name__


def scrubbed_traceback(exc: BaseException) -> str:
    """The traceback's FRAMES (file, line, function: diagnostic, never
    sensitive) followed by the scrubbed exception line instead of the raw one,
    so a log keeps what an operator needs to find the fault and drops what an
    upstream client may have interpolated into the message."""
    import traceback

    frames = "".join(traceback.format_tb(exc.__traceback__)) if exc.__traceback__ else ""
    return frames + scrub_error(exc)
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


#: A run of frames may drop a detection for a beat (OCR flicker, a replay that
#: hides the overlay, a face turning away); allow this many seconds of gap
#: before closing the run.
RUN_GAP_S = 3.0


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
        else:
            # Claim the reference in the SAME critical section that found the
            # entry. Releasing the lock before claiming leaves a window where
            # another thread's eviction can rmtree it, since it is not yet
            # marked in use.
            _FRAME_CACHE.move_to_end(key)
            _FRAME_USERS[key] = _FRAME_USERS.get(key, 0) + 1

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
        try:
            # Inside the cleanup block: this shells out to ffprobe, so a
            # KeyboardInterrupt here would otherwise leave the temp directory on
            # disk and the in-flight marker set, deadlocking later callers for
            # this media exactly as a failing mkdtemp would.
            timeout = _extract_timeout_s(media_path, ffprobe)
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
                    f"ffmpeg timed out after {exc.timeout:.0f}s on {media_path}"
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
            # Claim before releasing the lock, for the same reason: a fresh
            # extraction that is in the cache but not yet marked in use can be
            # evicted and deleted by another thread trimming to the cap.
            _FRAME_USERS[key] = _FRAME_USERS.get(key, 0) + 1
            _FRAME_INFLIGHT.discard(key)
            _evict_locked()
            _FRAME_LOCK.notify_all()
            cached = tmp

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


def appearance_text(label: str) -> str:
    """What an appearance cue says.

    A sighting is a statement about presence and nothing more, so the text
    says exactly that: the recognizer saw this person, and it does not know
    what they were doing. One definition, because face, jersey and scorebug all
    emit the same claim and a chunk is indexed on these exact words (#899).
    """
    return f"{label} on screen"


def collapse_sightings(
    sightings: list[tuple[float, str, float]],
    source: str,
    event: str,
    frame_gap: float,
    *,
    describe: Callable[[str], str],
) -> list[Cue]:
    """Collapse per-frame (t, entity_id, confidence) hits into per-run cues.

    Shared by every frame-sampling recognizer (scorebug, jersey, face).
    A run of consecutive sightings of one entity becomes one cue spanning
    first..last sighting (extended by one frame interval so a single-frame
    sighting still has nonzero duration); confidence is the run's max.

    `describe(entity_id)` supplies the cue text and is REQUIRED, with no
    default, because an empty text is not merely unhelpful: design 0004's
    response schema requires `text` with `minLength: 1`, and dir2mcp's ingest
    drops any annotation whose text is blank
    (`internal/ingest/recognize.go`). Every cue this function built used to
    carry no text at all, so a recognizer whose output nothing else
    corroborated was computed in full and then discarded before storage: face
    and jersey produced zero retrievable spans, and scorebug's `at_bat` cues
    survived only where play-by-play happened to supply the words for them
    (#899). A keyword with no default makes that failure impossible to
    reintroduce by omission.
    """
    runs: dict[str, list[tuple[float, float, float, float]]] = {}
    for t, pid, conf in sorted(sightings):
        pruns = runs.setdefault(pid, [])
        if pruns and t - pruns[-1][1] <= frame_gap + RUN_GAP_S:
            s, _, c, first = pruns[-1]
            pruns[-1] = (s, t, max(c, conf), first)
        else:
            pruns.append((t, t, conf, t))

    cues = []
    for pid, pruns in runs.items():
        for start, end, conf, _ in pruns:
            cues.append(
                Cue(
                    source=source,
                    start_s=start,
                    end_s=end + frame_gap,
                    event=event,
                    entity_ids=(pid,),
                    confidence=round(conf, 4),
                    text=describe(pid),
                )
            )
    cues.sort(key=lambda c: (c.start_s, c.entity_ids))
    return cues


#: How much of a read has to still be on screen for a run to stay open, as a
#: fraction of the shorter of the two token sequences. Measured on 180
#: consecutive frames of the TV Rain ticker (0.5s apart, real tesseract `rus`):
#: reads of the same scrolling passage score 0.53 or better even 4s apart,
#: while reads of unrelated passages never exceed 0.20. 0.5 sits in the middle
#: of that gap and reads naturally: half of what is on screen was on screen
#: when the run opened.
TEXT_RUN_SIMILARITY = 0.5

#: Below this many tokens a fuzzy comparison is not evidence of anything, so
#: the run test falls back to exact equality. A clock badge reads as two
#: tokens ("10:24 МСК"), and at 0.5 similarity "10:24 МСК" and "10:25 МСК"
#: would collapse into one run, silently erasing the minute that makes the
#: badge worth reading. This is the same refusal #639 made for short fuzzy
#: roster matches, for the same reason.
MIN_MATCH_TOKENS = 3


def text_tokens(text: str) -> tuple[str, ...]:
    """Whitespace tokens, NFKC-normalised, stripped of edge punctuation.

    Deliberately NOT `scorebug._fold`: that decomposes with NFKD and drops
    combining marks, which is right for Nuñez and wrong for Cyrillic, where it
    folds й onto и and ё onto е. NFKC composes instead, so no letter loses its
    identity, and case is handled at comparison time rather than here so the
    caller's text survives verbatim into the cue.

    Edge punctuation is stripped by Unicode category rather than by an ASCII
    list, so the Russian quotation marks «» and the em dash the ticker uses as
    a source separator come off exactly as an ASCII comma would. Inner
    punctuation stays: `40-километровой` is one token, not two.
    """
    out = []
    for raw in unicodedata.normalize("NFKC", text).split():
        start, end = 0, len(raw)
        while start < end and _is_punctuation(raw[start]):
            start += 1
        while end > start and _is_punctuation(raw[end - 1]):
            end -= 1
        if end > start and _has_content(raw[start:end]):
            out.append(raw[start:end])
    return tuple(out)


def _is_punctuation(char: str) -> bool:
    return unicodedata.category(char)[0] == "P"


def _has_content(token: str) -> bool:
    """Whether a token carries any letter or digit.

    Edge trimming alone does not clear symbol-only reads, because Unicode
    classes `|` and `+` as maths symbols (`Sm`), not punctuation. Tesseract
    emits a bare `|` for a column rule or a hard vertical edge often enough
    that, without this, a frame whose only "text" is the divider between two
    poll bars produces a text cue.
    """
    return any(char.isalnum() for char in token)


def _matched_tokens(a: tuple[str, ...], b: tuple[str, ...]) -> int:
    """Length of the longest common token subsequence.

    A true LCS rather than difflib's matching blocks. `SequenceMatcher` finds
    non-overlapping matching *blocks* by recursively taking the longest one
    first, which is a greedy choice and can miss a longer subsequence: for
    `b c a x b c` against `a b c` it reports 2, though every token of the
    shorter read appears in order in the longer and the answer is 3. That case
    is not hypothetical for a scroller, where a passage leaving the right edge
    can reappear at the left within one anchor's lifetime, and this function
    documents itself as a subsequence measure.

    Two rows rather than a full table: only the previous row is ever read, and
    reads here run to a few dozen tokens at most.
    """
    if not a or not b:
        return 0
    if len(a) < len(b):
        a, b = b, a
    previous = [0] * (len(b) + 1)
    for left in a:
        current = [0] * (len(b) + 1)
        for index, right in enumerate(b, start=1):
            if left == right:
                current[index] = previous[index - 1] + 1
            else:
                current[index] = max(previous[index], current[index - 1])
        previous = current
    return previous[-1]


def text_overlap(a: str, b: str) -> float:
    """How much of the shorter read is contained, in order, in the longer.

    1.0 when one read's tokens are a subsequence of the other's, which is the
    shape a scrolling overlay actually produces: consecutive frames hold a
    sliding window over one sentence, so they share a long ordered run of
    tokens and differ only at the clipped ends.

    Three measures were compared on 180 consecutive frames of the TV Rain
    ticker, OCR'd for real with tesseract `rus`: 1404 same-passage pairs (0.5s
    to 4s apart) against 7260 pairs at least 30s apart, which are unrelated
    passages. The reads are committed as
    `tests/fixtures/tvrain_ticker_measure.json` and every figure below is
    re-derived from them by `test_text_collapse.py`, because a previous version
    of this table carried a number that no longer matched the code and nothing
    recomputed it.

      | measure                       | same: med / worst | unrelated: med / worst |
      |-------------------------------|-------------------|------------------------|
      | longest common substring/min  | 0.882 / 0.378     | 0.053 / 0.132          |
      | difflib ratio over characters | 0.902 / 0.797     | 0.248 / 0.400          |
      | this: token LCS/min           | 0.778 / 0.455     | 0.000 / 0.231          |

    All three separate the two populations, so this is not a question of which
    one works. It is a question of dynamic range, because the run test compares
    every read against a fixed anchor and the score has to fall meaningfully as
    the content turns over, not just sit high and then drop off a cliff.

    Longest common substring, the intuitive choice for a sliding window, is
    ruled out by its spread rather than by its floor. OCR noise inside the
    window (`ЕВАМ` for `ЕКАМ`, `Навойне` for `На войне`) breaks the contiguous
    run, so two adjacent frames score anywhere from 0.378 to 1.000. A measure
    whose same-passage score varies by a factor of 2.6 at a fixed time
    separation cannot express "the content has turned over". A subsequence
    tolerates those breaks; a substring cannot. The same adjacent pairs under
    this measure span 0.636 to 1.000, a factor of 1.6.

    Character-level difflib is stable but has a high coincidence floor:
    unrelated Cyrillic reads share enough single characters by chance to score
    0.248 on median and 0.400 at worst. Against the anchor that leaves a usable
    band of about 0.40 to 0.90, a factor of 2.3. Tokens put the floor at 0.000
    median and 0.231 worst, giving a band of 0.23 to 0.78, a factor of 3.4, and
    the anchor decay lands inside it with room on both sides (median score
    against the run's first read: 0.82 at 1s, 0.50 at 10s, 0.33 at 15s, 0.00
    at 30s). Wider range means the default threshold is not balanced on a knife
    edge on a corpus nobody has measured yet.

    Normalising by the shorter sequence rather than by both (difflib's own
    `ratio`) is what keeps a partly-garbled or half-occluded read from breaking
    a run: a short correct read contained in a long one scores 1.0 instead of
    being penalised for the length it never had.

    Comparison is case-insensitive via `str.casefold`, which is correct for
    Cyrillic; the caller's original text is never modified.
    """
    ta = tuple(w.casefold() for w in text_tokens(a))
    tb = tuple(w.casefold() for w in text_tokens(b))
    shorter = min(len(ta), len(tb))
    if not shorter:
        return 0.0
    return _matched_tokens(ta, tb) / shorter


def _same_passage(
    anchor: tuple[str, ...], candidate: tuple[str, ...],
    similarity: float, min_tokens: int,
) -> bool:
    if anchor == candidate:
        return True  # a static overlay: identical reads, no fuzziness needed
    if len(anchor) < min_tokens or len(candidate) < min_tokens:
        return False
    shorter = min(len(anchor), len(candidate))
    return _matched_tokens(anchor, candidate) / shorter >= similarity


@dataclass
class _TextRun:
    """One open run of reads of the same passage, while it is being built."""

    start_s: float
    last_s: float
    confidence: float
    #: casefolded tokens of the run's FIRST read; every later read is compared
    #: against these, never against its predecessor. See collapse_text_sightings.
    anchor: tuple[str, ...]
    best_text: str


def collapse_text_sightings(
    sightings: list[tuple[float, str, float]],
    source: str,
    event: str,
    frame_gap: float,
    similarity: float = TEXT_RUN_SIMILARITY,
    min_tokens: int = MIN_MATCH_TOKENS,
    cue_gap: float | None = None,
) -> list[Cue]:
    """Collapse per-frame (t, text, confidence) reads into per-passage cues.

    The text counterpart of `collapse_sightings`, for overlays whose string
    changes from frame to frame while the thing on screen does not. A news
    ticker scrolls: the same headline is OCR'd at a different horizontal
    offset every frame, so every frame yields a different string and collapsing
    on identity emits one cue per frame. On 90s of TV Rain (180 frames, real
    tesseract) that is 180 near-duplicate cues for five headlines; this returns
    9, one per screenful, each carrying a readable window of the ticker.

    A read joins the open run when it lands within the same time gap
    `collapse_sightings` allows AND still overlaps the run's FIRST read by
    `similarity`. Anchoring on the first read rather than on the previous one
    is what bounds the run: chaining neighbour to neighbour is transitive, and
    a ticker that scrolls for 45 minutes chains into one 45-minute cue whose
    text is the whole programme (measured: chaining returns 1 cue for the same
    90s, at any threshold below 0.65). Against a fixed anchor a run ends when
    what is on screen has actually turned over, which for a scrolling overlay
    is the dwell time of a screenful and for a static one is never.

    That last property is why this needs no special case for static overlays:
    identical reads score 1.0 against the anchor forever, so a fixed banner
    collapses exactly as `collapse_sightings` would. It is also why the
    baseball path is untouched: it keeps calling `collapse_sightings`, which
    this function does not modify.

    The cue carries the run's longest read verbatim, not a reconstruction
    spliced from the windows. Splicing was built and measured against a
    hand-transcribed ground truth of the same 90s: token precision 0.836
    against 0.837 for the longest read, recall 0.896 against 0.875, and the
    extra recall is material the next run's cue already carries. It buys
    nothing and costs duplicated fragments at every splice point
    (`лидеры лидеры`, `погибших погибших`), so it is not shipped. A
    medoid-of-run selector scored 0.876 precision, four points better, at
    O(n^2) comparisons per run; that is not worth quadratic work in a run that
    can be thousands of frames long, but it is the first thing to try if the
    carried text is ever the weak link.

    `frame_gap` answers "how far apart may two reads be and still be the same
    sighting", and by default it also sets how far a cue runs past its last
    read. Those are the same number whenever reads arrive one sampling interval
    apart, which is why they were one parameter until a caller turned up where
    they differ. `OverlayReader` samples sparsely until its band search locks,
    so a news reader has to tolerate joins across the SEARCH stride while cues
    still end one FRAME later; passing `cue_gap` separates them. Widening
    `frame_gap` alone would push every cue's end past the start of the next
    one, which for a ticker means overlapping citations for consecutive
    headlines.

    Unlike `collapse_sightings` this walks one chronological channel and does
    not track several entities at once: an overlay band shows one string per
    frame, so interleaved sightings of two different passages are two runs,
    not two concurrent ones. Cues carry no entity ids, because ticker text is
    not an entity; note that `fusion.fuse` merges overlapping same-event cues
    that share no entity, so a consumer emitting these needs either distinct
    events or a fusion rule of its own.
    """
    runs: list[_TextRun] = []
    for t, text, conf in sorted(sightings):
        tokens = tuple(word.casefold() for word in text_tokens(text))
        if not tokens:
            continue  # a blank or all-punctuation read is not evidence of anything
        stripped = text.strip()
        open_run = runs[-1] if runs else None
        if (open_run is not None
                and t - open_run.last_s <= frame_gap + RUN_GAP_S
                and _same_passage(open_run.anchor, tokens, similarity, min_tokens)):
            open_run.last_s = t
            open_run.confidence = max(open_run.confidence, conf)
            if len(stripped) > len(open_run.best_text):
                open_run.best_text = stripped
        else:
            runs.append(_TextRun(start_s=t, last_s=t, confidence=conf,
                                 anchor=tokens, best_text=stripped))

    trailing = frame_gap if cue_gap is None else cue_gap
    if trailing < 0:
        # A cue may not end before its last observation: with one sighting a
        # negative extension puts end_s before start_s, which Cue refuses, and
        # with several it silently claims a span the overlay had already left.
        raise ValueError(f"cue_gap must not be negative: {trailing}")
    cues = [
        Cue(
            source=source,
            start_s=run.start_s,
            end_s=run.last_s + trailing,
            event=event,
            entity_ids=(),
            confidence=round(run.confidence, 4),
            text=run.best_text,
        )
        for run in runs
    ]
    cues.sort(key=lambda c: (c.start_s, c.text))
    return cues
