#!/usr/bin/env python3
"""Decode a 16 kHz mono WAV with facebook/mms-1b-all on CPU.

Runs under an interpreter that has torch, transformers and numpy (on q2e:
~/rfe-pilot/bakeoff-venv/bin/python). Prints one JSON document to stdout:

  {"model", "model_revision", "adapter", "transformers", "torch",
   "segments": [{"start", "end", "text", "words": [{"start","end","word"}]}]}

The audio is cut into fixed chunks anchored at 0 (default 30 s, matching the
rig's agreement grid) so a chunk boundary is also a window boundary. Word
times come from the CTC frame offsets (20 ms per frame). Same model and
adapter pattern as ~/rfe-pilot/mms_cpu.py; that script's 20 s chunks and
print-only output are replaced by timed JSON.

Set HF_HOME before running so the 4 GB checkpoint lands on the big disk.
"""
import argparse
import json
import os
import sys
import wave

MODEL = "facebook/mms-1b-all"


def load_wav(path):
    import numpy as np
    with wave.open(path, "rb") as w:
        if w.getnchannels() != 1 or w.getframerate() != 16000 or w.getsampwidth() != 2:
            raise SystemExit("expected 16 kHz mono 16-bit WAV")
        frames = w.readframes(w.getnframes())
    return np.frombuffer(frames, dtype=np.int16).astype(np.float32) / 32768.0


def model_revision(snapshot_dir_hint):
    # transformers keeps the resolved commit in the snapshot path name.
    parts = snapshot_dir_hint.split(os.sep)
    if "snapshots" in parts:
        i = parts.index("snapshots")
        if i + 1 < len(parts):
            return parts[i + 1]
    return ""


def cached_revision():
    """Commit of the cached checkpoint, without loading it."""
    try:
        from huggingface_hub import try_to_load_from_cache
        p = try_to_load_from_cache(MODEL, "config.json")
        return model_revision(p or "") if isinstance(p, str) else ""
    except Exception:  # pragma: no cover - best effort only
        return ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--audio")
    ap.add_argument("--adapter", required=True)
    ap.add_argument("--chunk-s", type=float, default=30.0)
    ap.add_argument("--threads", type=int, default=8)
    ap.add_argument("--version-only", action="store_true",
                    help="print model/library versions as JSON and exit; nothing is loaded")
    args = ap.parse_args()

    import torch
    import transformers

    if args.version_only:
        json.dump({"model": MODEL, "model_revision": cached_revision(), "adapter": args.adapter,
                   "transformers": transformers.__version__, "torch": torch.__version__},
                  sys.stdout)
        sys.stdout.write("\n")
        return
    if not args.audio:
        ap.error("--audio is required unless --version-only")

    from transformers import AutoProcessor, Wav2Vec2ForCTC

    torch.set_num_threads(max(1, args.threads))
    proc = AutoProcessor.from_pretrained(MODEL)
    model = Wav2Vec2ForCTC.from_pretrained(MODEL)
    proc.tokenizer.set_target_lang(args.adapter)
    model.load_adapter(args.adapter)
    model.eval()
    revision = cached_revision()

    audio = load_wav(args.audio)
    sr = 16000
    chunk = int(args.chunk_s * sr)
    ratio = float(getattr(model.config, "inputs_to_logits_ratio", 320))
    frame_s = ratio / sr
    segments = []
    n_chunks = (len(audio) + chunk - 1) // chunk
    for ci in range(n_chunks):
        seg = audio[ci * chunk:(ci + 1) * chunk]
        if seg.size < sr // 10:
            continue
        t0 = ci * args.chunk_s
        inp = proc(seg, sampling_rate=sr, return_tensors="pt")
        with torch.no_grad():
            logits = model(**inp).logits
        ids = torch.argmax(logits, dim=-1)
        text = proc.decode(ids[0]).strip()
        words = []
        try:
            dec = proc.tokenizer.decode(ids[0], output_word_offsets=True)
            for w in dec.word_offsets or []:
                words.append({
                    "start": round(t0 + w["start_offset"] * frame_s, 3),
                    "end": round(t0 + w["end_offset"] * frame_s, 3),
                    "word": w["word"],
                })
        except Exception as e:  # pragma: no cover - offsets are best effort
            sys.stderr.write(f"word offsets unavailable in chunk {ci}: {e}\n")
        end = t0 + seg.size / sr
        segments.append({"start": round(t0, 3), "end": round(end, 3), "text": text,
                         "words": words})
        if (ci + 1) % 10 == 0 or ci + 1 == n_chunks:
            sys.stderr.write(f"mms {args.adapter}: chunk {ci + 1}/{n_chunks}\n")
            sys.stderr.flush()

    json.dump({
        "model": MODEL, "model_revision": revision, "adapter": args.adapter,
        "transformers": transformers.__version__, "torch": torch.__version__,
        "chunk_s": args.chunk_s, "segments": segments,
    }, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
