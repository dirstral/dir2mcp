"""Transcript file format and the on-disk cache.

One transcript file per (recording, decoder id). The decoder id folds the
decoder name and its version, so a model upgrade or a different adapter is a
new cache entry and a re-run with the same decoder is a cache hit.

Format (rfe_rig.transcript.v1):
  recording       basename of the media file
  duration_s      length of the recording (ffprobe, or the decoder's report)
  decoder         {id, kind, name, model, version, detail}
  language        language the decoder reported, "" when it did not
  segments        [{start, end, text, words?: [{start, end, word}]}]
                  times in seconds, absolute to the recording start
"""
import datetime as _dt
import json
import os
import re
import subprocess

SCHEMA = "rfe_rig.transcript.v1"
_SLUG = re.compile(r"[^A-Za-z0-9._-]+")


def slug(text):
    s = _SLUG.sub("-", str(text)).strip("-")
    return s or "x"


def decoder_id(name, version):
    return f"{slug(name)}__{slug(version)}"


def recording_stem(path):
    return os.path.splitext(os.path.basename(path))[0]


def cache_path(results_dir, recording, dec_id):
    return os.path.join(results_dir, "transcripts", recording_stem(recording), dec_id + ".json")


def now_utc():
    return _dt.datetime.now(_dt.timezone.utc).replace(microsecond=0).isoformat()


def probe_duration_s(path):
    """Recording length via ffprobe. None when ffprobe is missing or fails."""
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "default=nw=1:nk=1", path],
            capture_output=True, text=True, check=True, timeout=120,
        ).stdout.strip()
        return float(out)
    except (OSError, subprocess.SubprocessError, ValueError):
        return None


def build(recording_path, duration_s, decoder, segments, language=""):
    kind = decoder["kind"]
    name = decoder["name"]
    version = decoder.get("version", "")
    dec = {
        "id": decoder_id(name, version),
        "kind": kind,
        "name": name,
        "model": decoder.get("model", ""),
        "version": version,
        "detail": decoder.get("detail", {}),
    }
    return {
        "schema": SCHEMA,
        "recording": os.path.basename(recording_path),
        "duration_s": duration_s,
        "decoder": dec,
        "language": language or "",
        "created_utc": now_utc(),
        "segments": [_clean_segment(s) for s in segments],
    }


def _clean_segment(seg):
    out = {
        "start": round(float(seg["start"]), 3),
        "end": round(float(seg["end"]), 3),
        "text": (seg.get("text") or "").strip(),
    }
    words = seg.get("words")
    if words:
        out["words"] = [
            {"start": round(float(w["start"]), 3), "end": round(float(w["end"]), 3),
             "word": (w.get("word") or "").strip()}
            for w in words if (w.get("word") or "").strip()
        ]
    return out


def save(transcript, path):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(transcript, fh, ensure_ascii=False, indent=0, separators=(",", ":"))
        fh.write("\n")
    os.replace(tmp, path)
    return path


def load(path):
    with open(path, encoding="utf-8") as fh:
        t = json.load(fh)
    if t.get("schema") != SCHEMA:
        raise ValueError(f"{path}: not a {SCHEMA} file")
    return t


def find_for(results_dir, recording, decoder_prefix):
    """All cached transcripts for a recording whose decoder id starts with prefix."""
    d = os.path.join(results_dir, "transcripts", recording_stem(recording))
    if not os.path.isdir(d):
        return []
    out = []
    for name in sorted(os.listdir(d)):
        if name.endswith(".json") and name.startswith(decoder_prefix):
            out.append(os.path.join(d, name))
    return out


def text_stats(transcript):
    segs = transcript.get("segments") or []
    words = sum(len(s.get("words") or []) for s in segs)
    chars = sum(len(s.get("text") or "") for s in segs)
    covered = sum(max(0.0, float(s["end"]) - float(s["start"])) for s in segs)
    return {"segments": len(segs), "words": words, "chars": chars,
            "segment_time_s": round(covered, 1)}
