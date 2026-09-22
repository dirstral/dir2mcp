"""CLI: python3 -m tools.rfe_rig <command> ...

Commands
  transcribe   decode recordings with a decoder spec into the transcript cache
  agreement    compare two cached decoders for a recording on a fixed grid
  coverage     read the dir2mcp state sqlite (copied first) for the baseline
  ask          the #964 answer-language measurement against a running daemon
  report       summary.md + summary.json over a results directory

Results live under --results (default tools/rfe_rig/results/<YYYY-MM-DD>).
"""
import argparse
import datetime as _dt
import json
import os
import sys

from . import agreement, ask, coverage, decoders, report, transcripts

HERE = os.path.dirname(os.path.abspath(__file__))


def default_results():
    return os.path.join(HERE, "results", _dt.date.today().isoformat())


def _dump(obj, path):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, ensure_ascii=False, indent=1)
        fh.write("\n")
    return path


def cmd_transcribe(a):
    # One identity probe (health, helper version) for the whole batch.
    dec = decoders.Decoder(a.decoder, mms_python=a.mms_python, fw_python=a.fw_python,
                           threads=a.threads, mms_chunk_s=a.mms_chunk_s)
    for rec in a.recording:
        t, path, cached = decoders.transcribe(a.decoder, rec, a.results, force=a.force, decoder=dec)
        st = transcripts.text_stats(t)
        print(f"{'cached' if cached else 'written'} {path}: {st['segments']} segments,"
              f" {st['words']} words, {st['chars']} chars, language={t.get('language') or '-'}")
    return 0


def _pick(results, recording, prefix):
    hits = transcripts.find_for(results, recording, prefix)
    if not hits:
        raise SystemExit(f"no cached transcript for {os.path.basename(recording)}"
                         f" with decoder prefix {prefix!r} under {results}")
    if len(hits) > 1:
        print(f"note: {len(hits)} transcripts match {prefix!r}; using {os.path.basename(hits[-1])}")
    return transcripts.load(hits[-1])


def cmd_agreement(a):
    ta = _pick(a.results, a.recording, a.a)
    tb = _pick(a.results, a.recording, a.b)
    rep = agreement.compare(ta, tb, window_s=a.window, threshold=a.threshold, worst=a.worst)
    path = os.path.join(a.results, "agreement",
                        agreement.output_name(a.recording, rep["a"], rep["b"]))
    _dump(rep, path)
    print(agreement.format_summary(rep))
    print(f"written {path}")
    return 0


def cmd_coverage(a):
    rep = coverage.run(a.state_dir, a.media_dir)
    path = _dump(rep, os.path.join(a.results, "coverage.json"))
    print(coverage.format_table(rep))
    print(f"written {path}")
    return 0


def cmd_ask(a):
    if a.replay:
        rep = ask.replay(a.replay)
        path = _dump(rep, os.path.join(a.results, "ask_replay.json"))
    else:
        token_file = a.token_file or os.environ.get("RFE_RIG_TOKEN_FILE")
        rep = ask.run(a.url, token_file, a.questions, runs=a.runs, timeout=a.timeout)
        path = _dump(rep, os.path.join(a.results, "ask.json"))
    print()
    print(ask.format_summary(rep))
    print(f"written {path}")
    return 0


def cmd_report(a):
    _s, md = report.write(a.results, summary_dir=a.summary_dir)
    print(md)
    for d in [a.results] + ([a.summary_dir] if a.summary_dir else []):
        print(f"written {os.path.join(d, 'summary.md')} summary.json agreement.csv")
    return 0


def main(argv=None):
    p = argparse.ArgumentParser(prog="rfe_rig", description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--results", default=default_results(),
                   help="results directory (default tools/rfe_rig/results/<today>)")
    sub = p.add_subparsers(dest="cmd", required=True)

    t = sub.add_parser("transcribe", help="decode recordings with a decoder spec")
    t.add_argument("--decoder", required=True,
                   help="http://host:port?model=..[&name=..] | sqlite:STATE_DIR | mms:ADAPTER | fw:MODEL_DIR[?language=xx]")
    t.add_argument("--force", action="store_true", help="ignore the cache and re-decode")
    t.add_argument("--mms-python", default="python3", help="interpreter with torch+transformers")
    t.add_argument("--fw-python", default="python3", help="interpreter with faster-whisper")
    t.add_argument("--threads", type=int, default=8, help="CPU threads for local decoders")
    t.add_argument("--mms-chunk-s", type=float, default=30.0)
    t.add_argument("recording", nargs="+")
    t.set_defaults(fn=cmd_transcribe)

    g = sub.add_parser("agreement", help="compare two cached decoders on one recording")
    g.add_argument("--recording", required=True)
    g.add_argument("--a", required=True, help="decoder id prefix of the reference side")
    g.add_argument("--b", required=True, help="decoder id prefix of the other side")
    g.add_argument("--window", type=float, default=agreement.DEFAULT_WINDOW_S)
    g.add_argument("--threshold", type=float, default=agreement.DEFAULT_THRESHOLD)
    g.add_argument("--worst", type=int, default=10)
    g.set_defaults(fn=cmd_agreement)

    c = sub.add_parser("coverage", help="baseline from the dir2mcp state sqlite")
    c.add_argument("--state-dir", required=True)
    c.add_argument("--media-dir", default=None, help="corpus root, for ffprobe durations")
    c.set_defaults(fn=cmd_coverage)

    q = sub.add_parser("ask", help="answer-language measurement (#964)")
    q.add_argument("--url", default="http://127.0.0.1:8791/mcp")
    q.add_argument("--token-file", default=None, help="bearer token file (or RFE_RIG_TOKEN_FILE)")
    q.add_argument("--questions", default=os.path.join(HERE, "questions.json"))
    q.add_argument("--runs", type=int, default=2)
    q.add_argument("--timeout", type=int, default=600)
    q.add_argument("--replay", default=None,
                   help="re-score a stored answer file instead of asking the daemon")
    q.set_defaults(fn=cmd_ask)

    r = sub.add_parser("report", help="write summary.md, summary.json and agreement.csv")
    r.add_argument("--summary-dir", default=None,
                   help="also write the three summary files here (e.g. tools/rfe_rig/baseline/<date>)")
    r.set_defaults(fn=cmd_report)

    a = p.parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    sys.exit(main())
