#!/usr/bin/env python3
"""Score a benchmark results file against the question set.

All rules are deterministic. No model judges an answer. bench/README.md
defines each metric.

Usage:
  score.py [--results FILE] [--questions FILE] [--json]
"""
import argparse
import json
import math
import os
import re
import sys
from collections import Counter

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from prepare import DEFAULT_WORK, normalize  # noqa: E402

# An answer abstains when it matches one of these patterns. The first two are
# the fixed texts that dir2mcp itself returns when retrieval finds nothing, or
# finds only weak evidence. The rest are the usual ways a model says that the
# context does not hold the answer.
ABSTAIN_PATTERNS = [
    r"\binsufficient evidence to answer\b",
    r"\bno relevant context found\b",
    r"\b(?:does|do|did) not (?:contain|provide|include|mention|specify|say|state|cover|"
    r"give|offer|address|discuss|describe|indicate|list|identify|name|have|explicitly)\b",
    r"\b(?:doesn't|don't) (?:contain|provide|include|mention|specify|say|state|cover|"
    r"give|offer|address|discuss|describe|indicate|list|identify|name|have|explicitly)\b",
    r"\bis not (?:mentioned|provided|specified|stated|included|covered|given|available|found|discussed|addressed)\b",
    r"\bare not (?:mentioned|provided|specified|stated|included|covered|given|available|found|discussed|addressed)\b",
    r"\bnot (?:mentioned|specified|stated|covered|discussed|addressed) in the (?:provided |given )?(?:context|documents?|sources?|texts?|passages?|corpus)\b",
    r"\bno (?:information|mention|details?|data|evidence|record|indication|reference)\b"
    r"(?: (?:is|was|are) (?:provided|given|available|found|present))?"
    r"(?: (?:about|on|regarding|of|in|that|to))?",
    r"\b(?:cannot|can ?not|can't|unable to) (?:be )?(?:answer|determine|find|identify|confirm|say|tell|provide)",
    r"\bi (?:do not|don't) know\b",
    r"\bthere is no (?:information|mention|data|evidence)\b",
]
_ABSTAIN_RE = re.compile("|".join(f"(?:{p})" for p in ABSTAIN_PATTERNS), re.IGNORECASE)


def abstained(answer):
    """True when the answer states that the corpus does not cover the question."""
    return bool(_ABSTAIN_RE.search(answer or ""))


def contains_gold(answer, golds):
    """True when the normalized answer holds a normalized gold answer as whole tokens."""
    hay = f" {normalize(answer or '')} "
    return any(normalize(g) and f" {normalize(g)} " in hay for g in golds)


def _token_scores(answer, gold):
    a, g = normalize(answer or "").split(), normalize(gold).split()
    if not a or not g:
        return 0.0, 0.0
    common = sum((Counter(a) & Counter(g)).values())
    if common == 0:
        return 0.0, 0.0
    recall = common / len(g)
    precision = common / len(a)
    return recall, 2 * precision * recall / (precision + recall)


def token_recall(answer, golds):
    """Best share of gold tokens that occur in the answer."""
    return max((_token_scores(answer, g)[0] for g in golds), default=0.0)


def token_f1(answer, golds):
    """Best SQuAD token F1 between the answer and a gold answer."""
    return max((_token_scores(answer, g)[1] for g in golds), default=0.0)


def citation_tag(citation):
    """Map a citations[] entry to the (rel_path, start_line, end_line) form of a tag."""
    span = citation.get("span") or {}
    if span.get("kind") != "lines":
        return (citation.get("rel_path") or "", None, None)
    try:
        return (citation.get("rel_path") or "", int(span["start_line"]), int(span["end_line"]))
    except (KeyError, TypeError, ValueError):
        return (citation.get("rel_path") or "", None, None)


# An inline tag is a bracketed citation in the answer text, such as
# [Normans.md:L53-L63], [Normans.md@L53-63], [Normans.md:L61] or [Normans.md].
_TAG_RE = re.compile(r"\[([^\[\]]+?\.md)(?:\s*(?::|@)\s*L(\d+)(?:\s*-\s*L?(\d+))?)?\s*\]", re.IGNORECASE)
# The attribution footer ("Sources: [a.md]") repeats the inline tags without a
# span. dir2mcp appends it, so it is not a separate claim of the model.
_FOOTER_RE = re.compile(r"(?im)^\s*(?:sources?|references?)\s*:.*$")


def inline_tags(answer):
    """Return (rel_path, start_line, end_line) for each inline tag in the answer body.

    start_line and end_line are None for a tag without a line span.
    """
    body = _FOOTER_RE.sub("", answer or "")
    out = []
    for m in _TAG_RE.finditer(body):
        rel = m.group(1).strip()
        start = int(m.group(2)) if m.group(2) else None
        end = int(m.group(3)) if m.group(3) else start
        out.append((rel, start, end))
    return out


def supports(tag, q, span_level=True):
    """True when the tag names the gold file and, at span level, its range holds the gold line."""
    rel, start, end = tag
    if not q.get("gold_rel_path") or os.path.basename(rel) != q["gold_rel_path"]:
        return False
    if not span_level:
        return True
    return start is not None and end is not None and start <= q["gold_line"] <= end


def percentile(values, p):
    """Nearest-rank percentile (p in 0..100) of a non-empty list."""
    if not values:
        return None
    s = sorted(values)
    rank = max(1, math.ceil(p / 100 * len(s)))
    return s[rank - 1]


def _ratio(num, den):
    return {"num": num, "den": den, "value": (num / den) if den else None}


def score(results, questions):
    qmap = {q["id"]: q for q in questions}
    rows = [r for r in results["results"] if r["id"] in qmap]
    ans = [r for r in rows if r["kind"] == "answerable"]
    unans_in = [r for r in rows if r["kind"] == "unanswerable_in_corpus"]
    unans_off = [r for r in rows if r["kind"] == "unanswerable_off_corpus"]

    correct = [r for r in ans if not r["is_error"] and contains_gold(r["answer"], qmap[r["id"]]["answers"])]

    def citation_stats(items_of, match):
        total = span = file_ = hit_span = hit_file = 0
        for r in ans:
            q = qmap[r["id"]]
            items = items_of(r)
            total += len(items)
            n_span = sum(match(c, q, True) for c in items)
            n_file = sum(match(c, q, False) for c in items)
            span += n_span
            file_ += n_file
            hit_span += n_span > 0
            hit_file += n_file > 0
        return {
            "precision_span": _ratio(span, total),
            "precision_file": _ratio(file_, total),
            "per_answer_mean": (total / len(ans)) if ans else None,
            "supporting_rate_span": _ratio(hit_span, len(ans)),
            "supporting_rate_file": _ratio(hit_file, len(ans)),
        }

    inline = citation_stats(lambda r: inline_tags(r["answer"]), supports)
    returned = citation_stats(lambda r: [citation_tag(c) for c in r["citations"]], supports)

    def abst(rs):
        return _ratio(sum(1 for r in rs if not r["is_error"] and abstained(r["answer"])), len(rs))

    lat = [r["latency_ms"] for r in rows]
    report = {
        "counts": {
            "answerable": len(ans),
            "unanswerable_in_corpus": len(unans_in),
            "unanswerable_off_corpus": len(unans_off),
            "tool_errors": sum(r["is_error"] for r in rows),
            "answerable_without_citations": sum(1 for r in ans if not r["citations"]),
            "server_evidence_insufficient": sum(1 for r in rows if r.get("evidence") == "insufficient"),
        },
        "answer_contains_gold": _ratio(len(correct), len(ans)),
        "answer_gold_token_recall_mean": (sum(token_recall(r["answer"], qmap[r["id"]]["answers"]) for r in ans) / len(ans)) if ans else None,
        "answer_token_f1_mean": (sum(token_f1(r["answer"], qmap[r["id"]]["answers"]) for r in ans) / len(ans)) if ans else None,
        "inline_citations": inline,
        "returned_citations": returned,
        "abstention_unanswerable_all": abst(unans_in + unans_off),
        "abstention_unanswerable_in_corpus": abst(unans_in),
        "abstention_unanswerable_off_corpus": abst(unans_off),
        "false_abstention_answerable": abst(ans),
        "latency_ms": {
            "p50": percentile(lat, 50), "p95": percentile(lat, 95),
            "max": max(lat) if lat else None, "n": len(lat),
        },
    }
    return report


def _fmt(r):
    if isinstance(r, dict):
        if r["value"] is None:
            return "n/a"
        return f"{100 * r['value']:.1f}% ({r['num']}/{r['den']})"
    if r is None:
        return "n/a"
    return f"{r:.3f}"


def render_markdown(report, run):
    c = report["counts"]
    lat = report["latency_ms"]
    il, rc = report["inline_citations"], report["returned_citations"]
    lines = [
        f"Run {run.get('utc')}: dir2mcp `{run.get('dir2mcp_version')}`, commit `{run.get('git_commit')}`.",
        f"Questions: {c['answerable']} answerable, {c['unanswerable_in_corpus']} unanswerable in corpus, "
        f"{c['unanswerable_off_corpus']} unanswerable off corpus. Tool errors: {c['tool_errors']}.",
        f"Index time: {run.get('index_seconds')} s. Ask time: {run.get('ask_seconds')} s. Total: {run.get('total_seconds')} s. ask k: {run.get('ask_k')}.",
        "",
        "| Metric | Value |",
        "| --- | --- |",
        f"| (a) Answer contains a gold answer | {_fmt(report['answer_contains_gold'])} |",
        f"| (a) Mean gold-token recall | {_fmt(report['answer_gold_token_recall_mean'])} |",
        f"| (a) Mean SQuAD token F1 (long answers score low) | {_fmt(report['answer_token_f1_mean'])} |",
        f"| (b) Inline citation precision, span level | {_fmt(il['precision_span'])} |",
        f"| (b) Inline citation precision, file level | {_fmt(il['precision_file'])} |",
        f"| (b) Mean inline citations for each answer | {_fmt(il['per_answer_mean'])} |",
        f"| (c) Answers with a supporting inline citation, span level | {_fmt(il['supporting_rate_span'])} |",
        f"| (c) Answers with a supporting inline citation, file level | {_fmt(il['supporting_rate_file'])} |",
        f"| (b) `citations[]` precision, span level | {_fmt(rc['precision_span'])} |",
        f"| (b) `citations[]` precision, file level | {_fmt(rc['precision_file'])} |",
        f"| (b) Mean `citations[]` entries for each answer | {_fmt(rc['per_answer_mean'])} |",
        f"| (c) Answers with a supporting `citations[]` entry, span level | {_fmt(rc['supporting_rate_span'])} |",
        f"| (c) Answers with a supporting `citations[]` entry, file level | {_fmt(rc['supporting_rate_file'])} |",
        f"| (d) Abstention, all unanswerable | {_fmt(report['abstention_unanswerable_all'])} |",
        f"| (d) Abstention, unanswerable in corpus (SQuAD 2.0 adversarial) | {_fmt(report['abstention_unanswerable_in_corpus'])} |",
        f"| (d) Abstention, unanswerable off corpus | {_fmt(report['abstention_unanswerable_off_corpus'])} |",
        f"| (d) False abstention, answerable | {_fmt(report['false_abstention_answerable'])} |",
        f"| (e) Latency p50 / p95 / max (ms, n={lat['n']}) | {lat['p50']} / {lat['p95']} / {lat['max']} |",
        f"| Answerable questions with no citation | {c['answerable_without_citations']} |",
        f"| Answers that dir2mcp withheld (`evidence` = insufficient) | {c['server_evidence_insufficient']} |",
    ]
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--results", default=os.path.join(DEFAULT_WORK, "results.json"))
    ap.add_argument("--questions", default=os.path.join(DEFAULT_WORK, "questions.jsonl"))
    ap.add_argument("--json", action="store_true", help="print the report as JSON")
    a = ap.parse_args()
    with open(a.results, encoding="utf-8") as f:
        results = json.load(f)
    with open(a.questions, encoding="utf-8") as f:
        questions = [json.loads(line) for line in f if line.strip()]
    report = score(results, questions)
    if a.json:
        print(json.dumps(report, indent=2))
    else:
        print(render_markdown(report, results["run"]), end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())
