"""Fixed windows in absolute recording time.

Both decoders are re-binned onto the same grid (default 30 s, anchored at 0)
so that a window compares the same stretch of audio. Text is placed by word
time when the decoder gave word timings. A segment without word timings has
its tokens spread across the segment span in proportion to token length (the
same estimate gigaam_server.py uses), then binned. Whole-segment placement was
rejected: dir2mcp's delivered chunks are about 34 s long, so "every segment
overlapping the window" would put 60 s of text against 30 s of the other side.
"""
import math

from .textnorm import tokens

DEFAULT_WINDOW_S = 30.0


def _timed_words(segment):
    """Yield (start_s, token) for each normalised token of one segment."""
    words = segment.get("words") or []
    if words:
        for w in words:
            for tok in tokens(w.get("word", "")):
                yield float(w["start"]), tok
        return
    toks = tokens(segment.get("text", ""))
    if not toks:
        return
    start = float(segment["start"])
    end = float(segment.get("end", start))
    span = max(end - start, 1e-3)
    total = sum(len(t) for t in toks) or 1
    t = start
    for tok in toks:
        yield t, tok
        t += span * (len(tok) / total)


def window_count(duration_s, window_s=DEFAULT_WINDOW_S):
    if window_s <= 0:
        raise ValueError("window_s must be positive")
    if duration_s <= 0:
        return 1
    return max(1, int(math.ceil(duration_s / window_s)))


def bin_tokens(segments, duration_s, window_s=DEFAULT_WINDOW_S):
    """Return a list of token lists, one per window covering [0, duration_s].

    A token at exactly the window boundary belongs to the later window. Tokens
    past duration_s land in the last window rather than being dropped.
    """
    n = window_count(duration_s, window_s)
    bins = [[] for _ in range(n)]
    for seg in segments:
        for start, tok in _timed_words(seg):
            idx = int(start // window_s)
            if idx < 0:
                idx = 0
            if idx >= n:
                idx = n - 1
            bins[idx].append(tok)
    return bins


def window_bounds(index, window_s=DEFAULT_WINDOW_S):
    return index * window_s, (index + 1) * window_s
