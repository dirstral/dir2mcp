#!/usr/bin/env python3
"""Run the end-to-end benchmark against a fresh dir2mcp index.

Steps:
  1. Build the corpus and the question set (prepare.py, checksum verified).
  2. Delete the old state dir and the old HOME, then start `dir2mcp up`.
  3. Wait until `dir2mcp status --json` reports a stopped run with pending=0.
  4. Ask every question through MCP (dir2mcp_ask over streamable HTTP).
  5. Stop the daemon, write <work>/results.json, and score it (score.py).

The daemon runs with a clean environment: only HOME (a new empty directory),
PATH and TERM. No provider key from your shell reaches it, so the run uses only
the providers in the config file.

Usage:
  run.py [--bin PATH] [--config FILE] [--work DIR] [--k N] [--limit N]
"""
import argparse
import datetime
import json
import os
import platform
import shutil
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(ROOT, "scripts"))

import prepare  # noqa: E402
import score  # noqa: E402
from release_smoke import HTTPClient  # noqa: E402


def daemon_env(home):
    path = os.pathsep.join(p for p in os.environ.get("PATH", "/usr/bin:/bin").split(os.pathsep) if p)
    return {"HOME": home, "PATH": path, "TERM": "dumb"}


def cli(binary, work, config, env, *args):
    cmd = [binary, "--dir", os.path.join(work, "corpus"), "--config", config,
           "--state-dir", os.path.join(work, "state"), "--non-interactive", *args]
    return subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=120)


def read_status(binary, work, config, env):
    r = cli(binary, work, config, env, "--json", "status")
    if r.returncode != 0:
        return None
    try:
        return json.loads(r.stdout)
    except json.JSONDecodeError:
        return None


def index_done(st):
    """True when the index run stopped with the WHOLE corpus indexed.

    That is: no errors, and every chunk embedded (embedded_ok == chunks_total).
    embedded_pending == 0 alone also holds for a chunk whose embedding failed,
    and a run with errors describes a partial corpus. A stopped run with errors
    is not "done"; wait_for_index stops on it at once.
    """
    if not st:
        return False
    ix = (st.get("snapshot") or {}).get("indexing") or {}
    total = ix.get("chunks_total", 0)
    return (ix.get("running") is False and ix.get("errors", 1) == 0
            and total > 0 and ix.get("embedded_ok") == total)


def index_failed(st):
    """True when the index run stopped with errors, so it will never be done."""
    ix = ((st or {}).get("snapshot") or {}).get("indexing") or {}
    return ix.get("running") is False and ix.get("errors", 0) > 0


def wait_for_index(binary, work, config, env, proc, timeout):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise SystemExit(f"dir2mcp up exited early with code {proc.returncode}; see {work}/up.log")
        st = read_status(binary, work, config, env)
        if st is not None:
            last = st
            if index_done(st):
                return st
            if index_failed(st):
                raise SystemExit(f"indexing stopped with errors; the corpus is partial, so no scores: {json.dumps(st)[:400]}")
        time.sleep(3)
    raise SystemExit(f"index did not finish in {timeout}s; last status: {json.dumps(last)[:400]}")


def wait_for_connection(state, proc, timeout=60):
    conn = os.path.join(state, "connection.json")
    tok = os.path.join(state, "secret.token")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise SystemExit(f"dir2mcp up exited early with code {proc.returncode}")
        if os.path.exists(conn) and os.path.exists(tok):
            with open(conn, encoding="utf-8") as f:
                url = json.load(f)["url"]
            with open(tok, encoding="utf-8") as f:
                return url, f.read().strip()
        time.sleep(1)
    raise SystemExit("no connection.json from dir2mcp up")


def text_of(result):
    for c in result.get("content") or []:
        if c.get("type") == "text":
            return c.get("text", "")
    return ""


def ask_all(client, questions, k):
    rows = []
    for i, q in enumerate(questions, 1):
        args = {"question": q["question"]}
        if k:
            args["k"] = k
        t0 = time.monotonic()
        try:
            r = client.call("dir2mcp_ask", args)
        except Exception as e:  # a transport failure is a result, not a crash
            r = {"isError": True, "content": [{"type": "text", "text": f"transport error: {e}"}]}
        ms = round((time.monotonic() - t0) * 1000)
        sc = r.get("structuredContent") or {}
        rows.append({
            "id": q["id"],
            "kind": q["kind"],
            "question": q["question"],
            "is_error": bool(r.get("isError")),
            "error_text": text_of(r)[:500] if r.get("isError") else None,
            "answer": sc.get("answer", ""),
            "citations": sc.get("citations") or [],
            "evidence": sc.get("evidence"),
            "hits": len(sc.get("hits") or []),
            "faithfulness": sc.get("faithfulness"),
            "latency_ms": ms,
        })
        print(f"[{i}/{len(questions)}] {q['kind']:<24} {ms:>6} ms  cites={len(rows[-1]['citations'])}"
              f"{'  ERROR' if rows[-1]['is_error'] else ''}", flush=True)
    return rows


def git_commit():
    try:
        return subprocess.run(["git", "-C", ROOT, "rev-parse", "HEAD"], capture_output=True,
                              text=True, timeout=10).stdout.strip() or None
    except Exception:
        return None


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--bin", default=os.path.join(ROOT, "dir2mcp"))
    ap.add_argument("--config", default=os.path.join(HERE, "config.local.yaml"))
    ap.add_argument("--work", default=prepare.DEFAULT_WORK)
    ap.add_argument("--k", type=int, default=0, help="ask k (0 = server default)")
    ap.add_argument("--limit", type=int, default=0, help="ask only the first N questions (smoke run)")
    ap.add_argument("--index-timeout", type=int, default=1800)
    a = ap.parse_args()

    binary = os.path.abspath(a.bin)
    config = os.path.abspath(a.config)
    work = os.path.abspath(a.work)
    if subprocess.run([sys.executable, os.path.join(HERE, "prepare.py"), "--work", work]).returncode != 0:
        return 1
    with open(os.path.join(work, "questions.jsonl"), encoding="utf-8") as f:
        questions = [json.loads(line) for line in f if line.strip()]
    if a.limit:
        questions = questions[: a.limit]

    state, home = os.path.join(work, "state"), os.path.join(work, "home")
    for d in (state, home):
        shutil.rmtree(d, ignore_errors=True)
    os.makedirs(home)
    env = daemon_env(home)
    version = subprocess.run([binary, "version"], env=env, capture_output=True, text=True).stdout.strip()

    started = time.monotonic()
    log = open(os.path.join(work, "up.log"), "w")
    proc = subprocess.Popen(
        [binary, "--dir", os.path.join(work, "corpus"), "--config", config, "--state-dir", state,
         "--non-interactive", "up"],
        env=env, cwd=work, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
    try:
        url, token = wait_for_connection(state, proc)
        st = wait_for_index(binary, work, config, env, proc, a.index_timeout)
        index_s = round(time.monotonic() - started, 1)
        print(f"index done in {index_s}s: {json.dumps(st['snapshot']['indexing'])}", flush=True)
        client = HTTPClient(url, token)
        stats = client.call("dir2mcp_stats", {}).get("structuredContent") or {}
        t_ask = time.monotonic()
        rows = ask_all(client, questions, a.k)
        ask_s = round(time.monotonic() - t_ask, 1)
    finally:
        cli(binary, work, config, env, "down")
        try:
            proc.wait(timeout=30)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()

    total_s = round(time.monotonic() - started, 1)
    results = {
        "run": {
            "utc": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "dir2mcp_version": version,
            "git_commit": git_commit(),
            "models": stats.get("models"),
            "indexing": stats.get("indexing"),
            "doc_counts": stats.get("doc_counts"),
            "ask_k": a.k or "server default",
            "index_seconds": index_s,
            "ask_seconds": ask_s,
            "total_seconds": total_s,
            "host": f"{platform.system()} {platform.machine()}",
            "questions": len(questions),
        },
        "results": rows,
    }
    out = os.path.join(work, "results.json")
    with open(out, "w", encoding="utf-8") as f:
        json.dump(results, f, indent=2, ensure_ascii=False)
    print(f"results -> {out}")
    report = score.score(results, questions)
    md = score.render_markdown(report, results["run"])
    with open(os.path.join(work, "summary.md"), "w", encoding="utf-8") as f:
        f.write(md)
    with open(os.path.join(work, "scores.json"), "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2)
    print(md)
    return 0


if __name__ == "__main__":
    sys.exit(main())
