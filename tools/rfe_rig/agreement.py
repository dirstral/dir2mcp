"""Cross-model agreement between two transcripts of one recording.

Both transcripts are binned onto the same fixed grid (windows.py). Per window:
  a_tokens, b_tokens   normalised token counts
  edits                token edit distance
  wer                  edits / a_tokens (decoder A is the reference)
  ned                  edits / max(a_tokens, b_tokens), symmetric in [0, 1]
  speech               at least one side has a token
  agree                speech and wer <= threshold

Summary:
  windows_total, speech_windows, agreeing_windows
  agree_fraction_all        agreeing / windows_total
  coverage_by_agreement     agreeing / speech_windows
  mean_wer, mean_ned        over speech windows
  overall_wer               sum(edits) / sum(a_tokens) over speech windows
  a_windows_with_text, b_windows_with_text

Relation to #566: that issue quoted "27% coverage" as the share of the audio
whose delivered transcript survived the quality gate (see coverage.py), and
"about 15% WER" as whisper-small-kyrgyz against MMS on six hand-picked clips.
Here the whole recording is scored on a fixed 30 s grid with the same token
normalisation, so the per-window WER is the same quantity computed everywhere
rather than on chosen clips.
"""
import os

from .textnorm import tokens
from .transcripts import decoder_id
from .wer import wer as _wer
from .windows import DEFAULT_WINDOW_S, bin_tokens, window_bounds

SCHEMA = "rfe_rig.agreement.v1"
DEFAULT_THRESHOLD = 0.5


def compare(ta, tb, window_s=DEFAULT_WINDOW_S, threshold=DEFAULT_THRESHOLD,
            duration_s=None, worst=10):
    dur = duration_s or max(float(ta.get("duration_s") or 0), float(tb.get("duration_s") or 0))
    if dur <= 0:
        dur = max(_last_end(ta), _last_end(tb))
    a_bins = bin_tokens(ta.get("segments") or [], dur, window_s)
    b_bins = bin_tokens(tb.get("segments") or [], dur, window_s)
    rows = []
    for i, (a, b) in enumerate(zip(a_bins, b_bins)):
        start, end = window_bounds(i, window_s)
        edits, w, ned = _wer(a, b)
        speech = bool(a or b)
        rows.append({
            "i": i, "start_s": start, "end_s": min(end, dur) if dur else end,
            "a_tokens": len(a), "b_tokens": len(b), "edits": edits,
            "wer": round(w, 4), "ned": round(ned, 4),
            "speech": speech, "agree": bool(speech and w <= threshold),
            "a_text": " ".join(a), "b_text": " ".join(b),
        })
    return {
        "schema": SCHEMA,
        "recording": ta.get("recording") or tb.get("recording"),
        "duration_s": dur,
        "window_s": window_s,
        "threshold": threshold,
        "a": ta["decoder"]["id"],
        "b": tb["decoder"]["id"],
        "summary": summarize(rows),
        "worst": worst_windows(rows, worst),
        "windows": rows,
    }


def _last_end(t):
    segs = t.get("segments") or []
    return max((float(s.get("end", 0)) for s in segs), default=0.0)


def summarize(rows):
    speech = [r for r in rows if r["speech"]]
    agreeing = [r for r in speech if r["agree"]]
    a_tok = sum(r["a_tokens"] for r in speech)
    edits = sum(r["edits"] for r in speech)
    n = len(rows)
    return {
        "windows_total": n,
        "speech_windows": len(speech),
        "agreeing_windows": len(agreeing),
        "agree_fraction_all": round(len(agreeing) / n, 4) if n else 0.0,
        "coverage_by_agreement": round(len(agreeing) / len(speech), 4) if speech else 0.0,
        "mean_wer": round(sum(r["wer"] for r in speech) / len(speech), 4) if speech else 0.0,
        "mean_ned": round(sum(r["ned"] for r in speech) / len(speech), 4) if speech else 0.0,
        "overall_wer": round(edits / a_tok, 4) if a_tok else (1.0 if speech else 0.0),
        "a_tokens": sum(r["a_tokens"] for r in rows),
        "b_tokens": sum(r["b_tokens"] for r in rows),
        "a_windows_with_text": sum(1 for r in rows if r["a_tokens"]),
        "b_windows_with_text": sum(1 for r in rows if r["b_tokens"]),
    }


def worst_windows(rows, n):
    speech = [r for r in rows if r["speech"]]
    # Worst by symmetric distance, then by how much text was at stake.
    speech.sort(key=lambda r: (-r["ned"], -(r["a_tokens"] + r["b_tokens"])))
    return [{k: r[k] for k in ("i", "start_s", "end_s", "a_tokens", "b_tokens", "wer", "ned")}
            for r in speech[:n]]


def output_name(recording, a_id, b_id):
    stem = os.path.splitext(os.path.basename(recording))[0]
    return f"{stem}__{a_id}__vs__{b_id}.json"


def format_summary(rep):
    s = rep["summary"]
    lines = [
        f"recording           {rep['recording']}",
        f"A (reference)       {rep['a']}",
        f"B                   {rep['b']}",
        f"window / threshold  {rep['window_s']:.0f} s / wer <= {rep['threshold']}",
        f"windows total       {s['windows_total']}",
        f"speech windows      {s['speech_windows']}  (A has text in {s['a_windows_with_text']}, B in {s['b_windows_with_text']})",
        f"agreeing windows    {s['agreeing_windows']}",
        f"agree fraction all  {s['agree_fraction_all']:.3f}",
        f"coverage by agreem. {s['coverage_by_agreement']:.3f}",
        f"mean WER / NED      {s['mean_wer']:.3f} / {s['mean_ned']:.3f}",
        f"overall WER         {s['overall_wer']:.3f}  (edits over A tokens, speech windows)",
        f"tokens A / B        {s['a_tokens']} / {s['b_tokens']}",
        "worst windows (by NED):",
    ]
    for w in rep["worst"]:
        lines.append(f"  #{w['i']:4d} {w['start_s']:7.0f}-{w['end_s']:<7.0f} a={w['a_tokens']:3d} b={w['b_tokens']:3d}"
                     f" wer={w['wer']:.2f} ned={w['ned']:.2f}")
    return "\n".join(lines)


__all__ = ["compare", "summarize", "worst_windows", "output_name", "format_summary",
           "decoder_id", "tokens"]
