#!/usr/bin/env python3
"""End-to-end MCP smoke test for a running dir2mcp instance.

Speaks the MCP streamable-HTTP protocol to a live daemon and exercises the
retrieval path the stas-legal guide's "Проверка" step covers — the exact
surface that kept breaking (empty embeddings, broken docling, BM25 NULL).

Gate (any failure -> exit 1):
  * stats: indexing stopped, errors==0, embedded_ok>0
  * ask (each question): non-empty answer AND >=1 citation, no tool error
  * search: >=1 hit
  * open_file(real pdf, page=1): non-empty text, not a tool error

Usage: release_smoke.py [--state-dir DIR] [--question Q ...]
"""
import argparse, json, os, sys, urllib.request

def _post(url, token, sid, body):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Content-Type", "application/json")
    req.add_header("MCP-Protocol-Version", "2025-11-25")
    req.add_header("Accept", "application/json, text/event-stream")
    if sid:
        req.add_header("Mcp-Session-Id", sid)
    resp = urllib.request.urlopen(req, timeout=120)
    hdr_sid = resp.headers.get("Mcp-Session-Id")
    out = None
    for line in resp.read().decode().splitlines():
        line = line.strip()
        if line.startswith("data:"):
            line = line[5:].strip()
        if not line:
            continue
        try:
            out = json.loads(line)
        except json.JSONDecodeError:
            continue
    return out, hdr_sid

def call(url, token, sid, name, args):
    body = {"jsonrpc": "2.0", "id": 99, "method": "tools/call",
            "params": {"name": name, "arguments": args}}
    d, _ = _post(url, token, sid, body)
    r = (d or {}).get("result", {})
    return r

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--state-dir", default=".dir2mcp")
    ap.add_argument("--question", action="append", default=[])
    a = ap.parse_args()
    conn = json.load(open(os.path.join(a.state_dir, "connection.json")))
    url = conn["url"]
    token = open(os.path.join(a.state_dir, "secret.token")).read().strip()
    questions = a.question or [
        "What powers does the BVI Financial Investigation Agency have? Cite sections.",
        "What entities are subject to BVI economic substance requirements?",
        "Under what circumstances must a financial institution file a suspicious transaction report in the BVI? Include the section number.",
    ]
    _, sid = _post(url, token, None, {"jsonrpc":"2.0","id":1,"method":"initialize",
        "params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"release-smoke","version":"1"}}})
    fails = []
    def check(name, ok, detail=""):
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}{(' — '+detail) if detail else ''}")
        if not ok: fails.append(name)

    print(f"dir2mcp release smoke @ {url}")
    # 1. stats
    r = call(url, token, sid, "dir2mcp_stats", {})
    ix = r.get("structuredContent", {}).get("indexing", {})
    check("stats: indexing stopped", ix.get("running") is False, f"running={ix.get('running')}")
    check("stats: errors==0", ix.get("errors", -1) == 0, f"errors={ix.get('errors')}")
    check("stats: embedded_ok>0", ix.get("embedded_ok", 0) > 0, f"embedded_ok={ix.get('embedded_ok')}")
    # 2. ask
    for q in questions:
        r = call(url, token, sid, "dir2mcp_ask", {"question": q, "k": 8})
        if r.get("isError"):
            check(f"ask: {q[:42]}…", False, "tool error"); continue
        sc = r.get("structuredContent", {})
        ans = (sc.get("answer") or "").strip()
        cites = sc.get("citations") or []
        check(f"ask: {q[:42]}…", bool(ans) and len(cites) >= 1, f"answer={len(ans)}c citations={len(cites)}")
    # 3. search
    r = call(url, token, sid, "dir2mcp_search", {"query": "financial investigation agency powers", "k": 5})
    hits = r.get("structuredContent", {}).get("hits", []) if not r.get("isError") else []
    check("search: >=1 hit", len(hits) >= 1, f"hits={len(hits)}")
    # 4. open_file (real pdf, page 1)
    lf = call(url, token, sid, "dir2mcp_list_files", {"glob": "*.pdf", "limit": 1})
    files = lf.get("structuredContent", {}).get("files", [])
    if files:
        rp = files[0]["rel_path"]
        of = call(url, token, sid, "dir2mcp_open_file", {"rel_path": rp, "page": 1})
        txt = (of.get("content") or [{}])[0].get("text", "") if not of.get("isError") else ""
        check(f"open_file page=1 ({rp[:30]}…)", not of.get("isError") and len(txt.strip()) > 0, "tool error" if of.get("isError") else f"{len(txt)}c")
    else:
        check("open_file: a pdf exists to test", False)

    print(f"\n{'✅ ALL PASS' if not fails else '❌ FAILED: ' + ', '.join(fails)}")
    sys.exit(1 if fails else 0)

if __name__ == "__main__":
    main()
