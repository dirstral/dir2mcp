"""Combine ask, coverage and agreement results into summary.md and summary.json.

Reads results/<date>/ask.json, coverage.json and agreement/*.json when present.
A missing input is reported as missing; nothing is invented.
"""
import glob
import json
import os

SCHEMA = "rfe_rig.summary.v1"


def _load(path):
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def build(results_dir):
    ask = _load(os.path.join(results_dir, "ask.json"))
    cov = _load(os.path.join(results_dir, "coverage.json"))
    agreements = []
    for p in sorted(glob.glob(os.path.join(results_dir, "agreement", "*.json"))):
        a = _load(p)
        if a:
            agreements.append({
                "file": os.path.basename(p), "recording": a["recording"], "a": a["a"], "b": a["b"],
                "window_s": a["window_s"], "threshold": a["threshold"], "summary": a["summary"],
            })
    transcripts = []
    for p in sorted(glob.glob(os.path.join(results_dir, "transcripts", "*", "*.json"))):
        t = _load(p)
        if t:
            segs = t.get("segments") or []
            transcripts.append({
                "recording": t["recording"], "decoder": t["decoder"]["id"],
                "kind": t["decoder"]["kind"], "language": t.get("language", ""),
                "segments": len(segs), "words": sum(len(s.get("words") or []) for s in segs),
                "chars": sum(len(s.get("text") or "") for s in segs),
            })
    replay = _load(os.path.join(results_dir, "ask_replay.json"))
    return {
        "schema": SCHEMA,
        "results_dir": results_dir,
        "ask": ask["summary"] if ask else None,
        "ask_runs": ask["runs"] if ask else None,
        "ask_replay": replay["summary"] if replay else None,
        "ask_replay_source": replay.get("replayed_from") if replay else None,
        "coverage": cov["recordings"] if cov else None,
        "agreements": agreements,
        "transcripts": transcripts,
    }


def _pct(x):
    return "-" if x is None else f"{100 * x:.1f}%"


def markdown(summary):
    lines = [f"# RFE validation rig summary", "", f"Results directory: `{summary['results_dir']}`", ""]
    lines += ["## Answer language (#964 port)", ""]
    a = summary["ask"]
    if a is None:
        lines.append("ask.json missing: not run.")
    else:
        lines.append(f"- {a['in_language']} of {a['asked']} answers in the language asked"
                     f" ({summary['ask_runs']} run(s) per question)")
        lines.append(f"- {a['tagged']} of {a['asked']} answers carry a citation tag")
        lines.append(f"- {a['undecidable']} undecidable by the detector, {a['errors']} errors")
        if a.get("fallback_answers"):
            lines.append(f"- {a['fallback_answers']} of {a['asked']} answers were retrieved-context"
                         " fallbacks (generator failed): the language score reflects the retrieved"
                         " chunks, not generated answers")
        for f in a["failing"]:
            lines.append(f"  - MISS want={f['want']} got={f['got']} run={f['run']}: {f['q']}")
    r = summary.get("ask_replay")
    if r is not None:
        lines += ["", f"Replay of stored answers `{summary.get('ask_replay_source')}`:"
                  f" {r['in_language']} of {r['asked']} in the language asked,"
                  f" {r['tagged']} of {r['asked']} with a citation tag."]
        for f in r["failing"]:
            lines.append(f"  - MISS want={f['want']} got={f['got']} run={f['run']}: {f['q']}")
    lines += ["", "## Coverage baseline (dir2mcp state)", ""]
    cov = summary["coverage"]
    if cov is None:
        lines.append("coverage.json missing: not run.")
    else:
        lines.append("| recording | language | source | conf | 8.6.13 coverage | language_covered"
                     " | live chunks | deleted | window mean s | window max s | delivered s | duration s | delivered %"
                     " | pre-#955 delivered % |")
        lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for r in cov:
            conf = r["language_confidence"]
            lines.append(
                f"| {os.path.basename(r['rel_path'])} | {r['language'] or '-'} | {r['language_source'] or '-'}"
                f" | {('%.2f' % conf) if isinstance(conf, (int, float)) else '-'}"
                f" | {'present' if r['coverage'] else 'absent'}"
                f" | {r['language_covered'] if r['language_covered'] is not None else 'absent'}"
                f" | {r['chunks_live']} | {r['chunks_deleted']} | {r['window_s']['mean']} | {r['window_s']['max']}"
                f" | {r['delivered_covered_s']} | {r['duration_s'] or '-'} | {_pct(r['delivered_coverage'])}"
                f" | {_pct(r.get('deleted_coverage'))} |")
    lines += ["", "## Cross-model agreement", ""]
    if not summary["agreements"]:
        lines.append("No agreement files.")
    else:
        lines.append("| recording | A (reference) | B | windows | speech | agreeing | agree all"
                     " | coverage by agreement | mean WER | overall WER | mean NED | A tokens | B tokens |")
        lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for g in summary["agreements"]:
            s = g["summary"]
            lines.append(
                f"| {g['recording']} | {g['a']} | {g['b']} | {s['windows_total']} | {s['speech_windows']}"
                f" | {s['agreeing_windows']} | {_pct(s['agree_fraction_all'])} | {_pct(s['coverage_by_agreement'])}"
                f" | {s['mean_wer']:.3f} | {s['overall_wer']:.3f} | {s['mean_ned']:.3f}"
                f" | {s['a_tokens']} | {s['b_tokens']} |")
    lines += ["", "## Transcripts on file", ""]
    if not summary["transcripts"]:
        lines.append("No transcripts.")
    else:
        lines.append("| recording | decoder | kind | language | segments | words | chars |")
        lines.append("|---|---|---|---|---|---|---|")
        for t in summary["transcripts"]:
            lines.append(f"| {t['recording']} | {t['decoder']} | {t['kind']} | {t['language'] or '-'}"
                         f" | {t['segments']} | {t['words']} | {t['chars']} |")
    lines.append("")
    return "\n".join(lines)


CSV_FIELDS = ["recording", "a", "b", "window_s", "threshold", "windows_total", "speech_windows",
              "agreeing_windows", "agree_fraction_all", "coverage_by_agreement", "mean_wer",
              "overall_wer", "mean_ned", "a_tokens", "b_tokens"]


def agreement_csv(summary):
    """One row per scored decoder pair; small enough to track in git."""
    import csv
    import io
    buf = io.StringIO()
    w = csv.DictWriter(buf, fieldnames=CSV_FIELDS, lineterminator="\n")
    w.writeheader()
    for g in summary["agreements"]:
        row = {"recording": g["recording"], "a": g["a"], "b": g["b"],
               "window_s": g["window_s"], "threshold": g["threshold"]}
        row.update({k: g["summary"][k] for k in CSV_FIELDS if k in g["summary"]})
        w.writerow(row)
    return buf.getvalue()


def write(results_dir, summary_dir=None):
    """Write summary.json, summary.md and agreement.csv.

    They go into results_dir and, when summary_dir is given, also there. The
    summary files are the tracked artefact; the transcripts and per-window
    agreement files they summarise stay where the run put them.
    """
    s = build(results_dir)
    md = markdown(s)
    csv_text = agreement_csv(s)
    targets = [results_dir] + ([summary_dir] if summary_dir else [])
    for d in targets:
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "summary.json"), "w", encoding="utf-8") as fh:
            json.dump(s, fh, ensure_ascii=False, indent=1)
            fh.write("\n")
        with open(os.path.join(d, "summary.md"), "w", encoding="utf-8") as fh:
            fh.write(md)
        with open(os.path.join(d, "agreement.csv"), "w", encoding="utf-8") as fh:
            fh.write(csv_text)
    return s, md
