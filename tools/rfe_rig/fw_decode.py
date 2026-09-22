#!/usr/bin/env python3
"""Decode a 16 kHz mono WAV with a local CTranslate2 whisper model on CPU.

Runs under an interpreter that has faster-whisper (on q2e:
~/rfe-pilot/stt-venv/bin/python). Prints one JSON document to stdout with the
same shape as mms_decode.py. This is how the #566 Kyrgyz specialist
(UlutSoftLLC/whisper-small-kyrgyz, converted to CT2) is scored as a third
decoder without touching the shared GPU. The decode settings mirror the
running whisper servers: VAD on, temperature ladder, no conditioning on the
previous text (the anti-repetition guard).
"""
import argparse
import json
import sys


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--audio")
    ap.add_argument("--model-dir")
    ap.add_argument("--language", default="")
    ap.add_argument("--threads", type=int, default=8)
    ap.add_argument("--compute-type", default="int8")
    ap.add_argument("--version-only", action="store_true",
                    help="print library version as JSON and exit; nothing is loaded")
    args = ap.parse_args()

    import faster_whisper

    if args.version_only:
        json.dump({"faster_whisper": getattr(faster_whisper, "__version__", ""),
                   "compute_type": args.compute_type}, sys.stdout)
        sys.stdout.write("\n")
        return
    if not args.audio or not args.model_dir:
        ap.error("--audio and --model-dir are required unless --version-only")

    from faster_whisper import WhisperModel

    model = WhisperModel(args.model_dir, device="cpu", compute_type=args.compute_type,
                         cpu_threads=max(1, args.threads))
    segments_iter, info = model.transcribe(
        args.audio,
        language=args.language or None,
        task="transcribe",
        temperature=(0.0, 0.2, 0.4, 0.6, 0.8, 1.0),
        condition_on_previous_text=False,
        word_timestamps=True,
        vad_filter=True,
    )
    segments = []
    for i, s in enumerate(segments_iter):
        seg = {"start": round(s.start, 3), "end": round(s.end, 3), "text": s.text}
        if s.words:
            seg["words"] = [{"start": round(w.start, 3), "end": round(w.end, 3), "word": w.word}
                            for w in s.words]
        segments.append(seg)
        if (i + 1) % 50 == 0:
            sys.stderr.write(f"fw: {i + 1} segments, at {s.end:.0f}s\n")
            sys.stderr.flush()
    json.dump({
        "model_dir": args.model_dir, "language": info.language,
        "language_probability": getattr(info, "language_probability", None),
        "faster_whisper": getattr(faster_whisper, "__version__", ""),
        "compute_type": args.compute_type, "duration": float(info.duration),
        "segments": segments,
    }, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
