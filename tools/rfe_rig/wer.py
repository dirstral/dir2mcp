"""Token-level word error rate.

WER is edit distance over tokens divided by the reference length. Agreement
between two decoders has no ground truth, so the rig names decoder A the
reference and also reports the symmetric normalised edit distance, which is
bounded in [0, 1] and does not depend on which side is called reference.
"""


def edit_distance(a, b):
    """Levenshtein distance between two token sequences."""
    if a == b:
        return 0
    if not a:
        return len(b)
    if not b:
        return len(a)
    prev = list(range(len(b) + 1))
    for i in range(1, len(a) + 1):
        cur = [i] + [0] * len(b)
        ai = a[i - 1]
        for j in range(1, len(b) + 1):
            cost = 0 if ai == b[j - 1] else 1
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + cost)
        prev = cur
    return prev[len(b)]


def wer(ref, hyp):
    """Return (edits, wer, ned).

    wer  = edits / len(ref); 1.0 when ref is empty and hyp is not, 0.0 when
           both are empty.
    ned  = edits / max(len(ref), len(hyp)); symmetric, bounded in [0, 1].
    """
    edits = edit_distance(ref, hyp)
    longest = max(len(ref), len(hyp))
    if longest == 0:
        return 0, 0.0, 0.0
    ned = edits / longest
    if not ref:
        return edits, 1.0, ned
    return edits, edits / len(ref), ned
