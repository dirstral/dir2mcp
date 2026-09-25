#!/usr/bin/env python3
"""Fetch the pinned dataset, verify its checksum, and build the corpus and questions.

The script reads bench/manifest.json. It downloads the SQuAD 2.0 dev set once
into <work>/cache, and it refuses a file whose sha256 differs from the pin.

Outputs:
  <work>/corpus/<slug>.md   one Markdown file for each corpus article.
                            Line 1 is the title. Each paragraph is one line,
                            and a blank line separates paragraphs.
  <work>/questions.jsonl    one JSON object for each question (see README).

The selection is deterministic. Questions sort by the sha256 of their SQuAD id,
so the same pin always gives the same question set on any Python version.

Usage:
  prepare.py [--work DIR] [--source FILE]
"""
import argparse
import hashlib
import json
import os
import re
import string
import sys
import unicodedata
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_WORK = os.path.join(HERE, "work")
_ASCII_PUNCT = set(string.punctuation)


def slug(title):
    """Map a SQuAD article title to a plain file stem."""
    return re.sub(r"[^A-Za-z0-9]+", "_", title).strip("_")


def normalize(text):
    """SQuAD answer normalization: lower case, no punctuation, no articles.

    The SQuAD script removes only ASCII punctuation. This version also removes
    Unicode punctuation (for example curly quotes and en dashes), so that
    'often damaging' in curly quotes matches the gold text often damaging.
    """
    text = text.lower()
    text = "".join(ch for ch in text
                   if ch not in _ASCII_PUNCT and not unicodedata.category(ch).startswith("P"))
    text = re.sub(r"\b(a|an|the)\b", " ", text)
    return " ".join(text.split())


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def fetch(source, dest):
    """Download the pinned file to dest unless a verified copy is there."""
    if os.path.exists(dest) and sha256_file(dest) == source["sha256"]:
        return dest
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    print(f"fetch {source['url']}")
    with urllib.request.urlopen(source["url"], timeout=120) as resp, open(tmp, "wb") as out:
        while True:
            block = resp.read(1 << 20)
            if not block:
                break
            out.write(block)
    got = sha256_file(tmp)
    if got != source["sha256"]:
        os.remove(tmp)
        raise SystemExit(f"checksum mismatch: got {got}, want {source['sha256']}")
    os.replace(tmp, dest)
    return dest


def qkey(q):
    return hashlib.sha256(q["id"].encode()).hexdigest()


def write_article(article, corpus_dir):
    """Write one article and return the 1-based line number of each paragraph."""
    title = article["title"].replace("_", " ")
    lines = [f"# {title}"]
    para_lines = []
    for p in article["paragraphs"]:
        lines.append("")
        # A paragraph must stay on one line, so the gold span is one line.
        lines.append(" ".join(p["context"].split()))
        para_lines.append(len(lines))
    rel = slug(article["title"]) + ".md"
    with open(os.path.join(corpus_dir, rel), "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    return rel, para_lines


def gold_answers(q):
    seen, out = set(), []
    for a in q.get("answers") or []:
        t = a["text"].strip()
        if t and t not in seen:
            seen.add(t)
            out.append(t)
    return out


def build(data, manifest, work):
    by_title = {a["title"]: a for a in data["data"]}
    sample = manifest["sample"]
    corpus_dir = os.path.join(work, "corpus")
    os.makedirs(corpus_dir, exist_ok=True)
    for name in os.listdir(corpus_dir):
        os.remove(os.path.join(corpus_dir, name))

    questions, corpus_norm = [], []
    for title in manifest["corpus_articles"]:
        article = by_title[title]
        rel, para_lines = write_article(article, corpus_dir)
        corpus_norm.append(" " + normalize(" ".join(p["context"] for p in article["paragraphs"])) + " ")
        qas = [(pi, q) for pi, p in enumerate(article["paragraphs"]) for q in p["qas"]]
        qas.sort(key=lambda t: qkey(t[1]))
        ans = [t for t in qas if not t[1]["is_impossible"]][: sample["answerable_per_article"]]
        imp = [t for t in qas if t[1]["is_impossible"]][: sample["unanswerable_in_corpus_per_article"]]
        for pi, q in ans:
            questions.append({
                "id": q["id"], "kind": "answerable", "article": title,
                "question": q["question"], "answers": gold_answers(q),
                "gold_rel_path": rel, "gold_line": para_lines[pi],
            })
        for pi, q in imp:
            questions.append({
                "id": q["id"], "kind": "unanswerable_in_corpus", "article": title,
                "question": q["question"], "answers": [],
                "gold_rel_path": None, "gold_line": None,
            })

    corpus_blob = "".join(corpus_norm)
    for title in manifest["off_corpus_articles"]:
        article = by_title[title]
        qas = [q for p in article["paragraphs"] for q in p["qas"] if not q["is_impossible"]]
        qas.sort(key=qkey)
        picked = 0
        for q in qas:
            if picked == sample["off_corpus_per_article"]:
                break
            # Keep a question only when no gold answer occurs in the corpus, so
            # the corpus cannot support the answer by accident.
            if any(f" {normalize(a)} " in corpus_blob for a in gold_answers(q)):
                continue
            questions.append({
                "id": q["id"], "kind": "unanswerable_off_corpus", "article": title,
                "question": q["question"], "answers": [],
                "reference_answers_outside_corpus": gold_answers(q),
                "gold_rel_path": None, "gold_line": None,
            })
            picked += 1
    return questions


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--work", default=DEFAULT_WORK)
    ap.add_argument("--source", help="use this local copy of the dataset (its checksum must match)")
    a = ap.parse_args()

    with open(os.path.join(HERE, "manifest.json"), encoding="utf-8") as f:
        manifest = json.load(f)
    src = manifest["source"]
    if a.source:
        got = sha256_file(a.source)
        if got != src["sha256"]:
            raise SystemExit(f"checksum mismatch for {a.source}: got {got}, want {src['sha256']}")
        path = a.source
    else:
        path = fetch(src, os.path.join(a.work, "cache", os.path.basename(src["url"])))

    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    questions = build(data, manifest, a.work)
    out = os.path.join(a.work, "questions.jsonl")
    with open(out, "w", encoding="utf-8") as f:
        for q in questions:
            f.write(json.dumps(q, ensure_ascii=False) + "\n")
    kinds = {}
    for q in questions:
        kinds[q["kind"]] = kinds.get(q["kind"], 0) + 1
    print(f"corpus: {len(manifest['corpus_articles'])} files in {os.path.join(a.work, 'corpus')}")
    print(f"questions: {len(questions)} {kinds} -> {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
