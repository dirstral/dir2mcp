"""Answer-language fidelity (#964), ported from measure.py ask_report.

Every question in the question file is asked `runs` times. The answer language
is judged by detect.detect (the detector that scored 24 of 24 on q2e), and the
citation count is the number of [file@t=m:ss] tags. The summary is
"N of M answers in the language asked".

Two things measure.py did not record are kept here because they changed the
meaning of a run on 2026-09-22:
  * everything the tool returned besides the answer text (structured
    citations, evidence, faithfulness) goes into the row;
  * an answer the daemon produced by falling back to retrieved context when
    the generator failed ("Question: ... Top context: ...") is flagged
    generated=false. Such an answer is in the language of the retrieved
    chunks, not of a model, so the run does not measure what #964 measured.

`replay` re-scores a stored answer file (measure.py or rig format) without a
daemon. That is how the #964 fixture is reproduced from committed inputs.
"""
import json
import os

from .detect import citation_tags, detect
from .mcp_client import MCP, read_token

SCHEMA = "rfe_rig.ask.v1"


def load_questions(path):
    with open(path, encoding="utf-8") as fh:
        qs = json.load(fh)
    out = []
    for q in qs:
        if "lang" not in q or "q" not in q:
            raise SystemExit(f"{path}: every question needs 'lang' and 'q'")
        out.append((q["lang"], q["q"]))
    return out


def is_fallback(answer, structured=None):
    """True when the answer is retrieved context published in place of a
    generated answer.

    A daemon built after dir2mcp #1019 says so itself: SPEC 9.4.5 marks such an
    answer `answer_source: retrieval_only` in the structured content, and that
    field is authoritative whenever it is present (`generated` or absent means
    a model produced the text). An older daemon carries no `answer_source`, so
    the text shape of its fallback ("Question: ... Top context: ...") is the
    only evidence and is used then.
    """
    source = str((structured or {}).get("answer_source") or "").strip().lower()
    if source:
        return source == "retrieval_only"
    a = (answer or "").lstrip()
    return a.startswith("Question:") and "Top context:" in a


def score(want, answer, structured=None):
    got, why = detect(answer)
    return {
        "want": want, "got": got, "why": why,
        "tags": citation_tags(answer),
        "citations_n": len((structured or {}).get("citations") or []),
        "generated": not is_fallback(answer, structured),
        "answer_source": (structured or {}).get("answer_source"),
    }


def run(url, token_file, questions_file, runs=2, timeout=600, log=print):
    questions = load_questions(questions_file)
    client = MCP(url, read_token(token_file)).connect()
    rows = []
    log(f"{'asked':5s} {'got':8s} {'tags':>4s} {'sec':>6s}  question / answer head")
    for want, q in questions:
        for run_i in range(runs):
            answer, dt, err, sc = client.ask(q, timeout=timeout)
            structured = {k: v for k, v in (sc or {}).items() if k != "answer"}
            if err:
                log(f"{want:5s} {'error':8s} {'':>4s} {dt:6.1f}  {q[:40]} -> {str(err)[:90]}")
                rows.append({"want": want, "got": "error", "why": str(err), "tags": 0,
                             "citations_n": 0, "generated": False, "sec": round(dt, 1),
                             "run": run_i + 1, "q": q, "answer": "", "structured": structured})
                continue
            row = score(want, answer, structured)
            flag = " " if row["got"] == want else "*"
            gen = "" if row["generated"] else " [fallback]"
            log(f"{want:5s} {row['got']:8s}{flag}{row['tags']:3d} {dt:6.1f}  {q[:40]:40s}"
                f" | {' '.join(answer.split())[:70]}{gen}")
            row.update({"sec": round(dt, 1), "run": run_i + 1, "q": q, "answer": answer,
                        "structured": structured})
            rows.append(row)
    return {"schema": SCHEMA, "url": url, "questions_file": questions_file, "runs": runs,
            "summary": summarize(rows), "rows": rows}


def replay(path):
    """Re-score a stored answer file. Accepts measure.py rows (want, q, answer)
    or a rig ask.json (with a 'rows' key)."""
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
    stored = data["rows"] if isinstance(data, dict) else data
    rows, seen = [], {}
    for r in stored:
        seen[r["q"]] = seen.get(r["q"], 0) + 1
        row = score(r["want"], r.get("answer", ""), r.get("structured"))
        row.update({"sec": r.get("sec"), "run": r.get("run", seen[r["q"]]), "q": r["q"],
                    "answer": r.get("answer", ""), "stored_got": r.get("got")})
        rows.append(row)
    return {"schema": SCHEMA, "replayed_from": os.path.basename(path), "runs": None,
            "summary": summarize(rows), "rows": rows}


def summarize(rows):
    n = len(rows)
    ok = sum(1 for r in rows if r["got"] == r["want"])
    return {
        "asked": n,
        "in_language": ok,
        "undecidable": sum(1 for r in rows if r["got"] in ("unknown", "empty")),
        "errors": sum(1 for r in rows if r["got"] == "error"),
        "tagged": sum(1 for r in rows if r["tags"] > 0),
        "cited_structured": sum(1 for r in rows if r.get("citations_n", 0) > 0),
        "fallback_answers": sum(1 for r in rows if r.get("generated") is False and r["got"] != "error"),
        "failing": [{"want": r["want"], "got": r["got"], "q": r["q"], "run": r["run"]}
                    for r in rows if r["got"] != r["want"]],
    }


def format_summary(rep):
    s = rep["summary"]
    lines = [f"LANGUAGE  {s['in_language']}/{s['asked']} answers in the language asked"
             f" ({s['undecidable']} undecidable by this detector, {s['errors']} errors)",
             f"CITATIONS {s['tagged']}/{s['asked']} answers carry at least one [file@t=] tag;"
             f" {s['cited_structured']}/{s['asked']} carry structured citations"]
    if s["fallback_answers"]:
        lines.append(f"FALLBACK  {s['fallback_answers']}/{s['asked']} answers are retrieved context,"
                     f" not generated (the daemon's generator failed); the language score"
                     f" then reflects the retrieved chunks, not #964's generated answers")
    for f in s["failing"]:
        lines.append(f"  MISS want={f['want']} got={f['got']} run={f['run']} {f['q'][:60]}")
    return "\n".join(lines)
