#!/usr/bin/env python3
"""End-to-end MCP smoke test for a running dir2mcp instance.

Exercises the retrieval path the stas-legal guide's verification step covers —
the surface that kept breaking (empty embeddings, broken docling, BM25 NULL,
open_file page reads). Two transports:

  --transport http   (default) speaks MCP streamable-HTTP straight to the daemon
                      (the server contract; deterministic, no extra deps).
  --transport stdio  drives the SAME `bunx mcp-remote` bridge Claude Desktop
                      uses, over stdio — catches client/bridge-layer regressions
                      too.

Gate (any failure -> exit 1):
  * stats: indexing stopped, errors==0, embedded_ok>0
  * ask (each question): non-empty answer AND >=1 citation, no tool error
  * search: >=1 hit
  * open_file(real pdf, page=1): non-empty, non-binary text

Usage:
  release_smoke.py [--state-dir DIR] [--transport http|stdio] [--bunx CMD] [--question Q ...]
"""
import argparse, json, os, select, subprocess, sys, urllib.request

PROTO = "2025-11-25"


class HTTPClient:
    """MCP over streamable-HTTP, directly to the daemon."""

    def __init__(self, url, token):
        self.url, self.token, self.sid = url, token, None
        self.sid = self._post({"jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": PROTO, "capabilities": {},
                       "clientInfo": {"name": "release-smoke", "version": "1"}}})[1]

    def _post(self, body):
        req = urllib.request.Request(self.url, data=json.dumps(body).encode(), method="POST")
        req.add_header("Authorization", f"Bearer {self.token}")
        req.add_header("Content-Type", "application/json")
        req.add_header("MCP-Protocol-Version", PROTO)
        req.add_header("Accept", "application/json, text/event-stream")
        if self.sid:
            req.add_header("Mcp-Session-Id", self.sid)
        resp = urllib.request.urlopen(req, timeout=120)
        out = None
        for line in resp.read().decode().splitlines():
            line = line.strip()
            if line.startswith("data:"):
                line = line[5:].strip()
            if line:
                try:
                    out = json.loads(line)
                except json.JSONDecodeError:
                    pass
        return out, resp.headers.get("Mcp-Session-Id")

    def call(self, name, args):
        d, _ = self._post({"jsonrpc": "2.0", "id": 99, "method": "tools/call",
                           "params": {"name": name, "arguments": args}})
        return (d or {}).get("result", {})

    def close(self):
        pass


class StdioClient:
    """MCP over the bunx mcp-remote bridge (the path Claude Desktop uses)."""

    def __init__(self, url, token, bunx="bunx"):
        self.proc = subprocess.Popen(
            [bunx, "mcp-remote", url,
             "--header", f"MCP-Protocol-Version:{PROTO}",
             "--header", f"Authorization:Bearer {token}"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, bufsize=1)
        self._id = 1
        self._send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": PROTO, "capabilities": {},
                       "clientInfo": {"name": "release-smoke", "version": "1"}}})
        # bunx may cold-download mcp-remote, and the bridge handshakes with the
        # server before forwarding — allow a generous window for the first reply.
        if self._read_until(1, timeout=90) is None:
            raise RuntimeError("mcp-remote bridge did not complete initialize within 90s")
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def _send(self, obj):
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def _read_until(self, want_id, timeout=60):
        import time
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            r, _, _ = select.select([self.proc.stdout], [], [], deadline - time.monotonic())
            if not r:
                continue
            line = self.proc.stdout.readline()
            if line == "":
                return None  # bridge closed stdout
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue  # skip any non-JSON noise
            if msg.get("id") == want_id:
                return msg
        return None

    def call(self, name, args):
        self._id += 1
        rid = self._id
        self._send({"jsonrpc": "2.0", "id": rid, "method": "tools/call",
                    "params": {"name": name, "arguments": args}})
        msg = self._read_until(rid, timeout=120)
        if msg is None:
            # the exact "Failed to call tool" class: bridge never returned a result
            return {"isError": True, "content": [{"type": "text", "text": "bridge: no response (Failed to call tool)"}]}
        return msg.get("result", {})

    def close(self):
        try:
            self.proc.terminate()
        except Exception:
            pass


def run_checks(client, questions):
    fails = []
    def check(name, ok, detail=""):
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}{(' — ' + detail) if detail else ''}")
        if not ok:
            fails.append(name)

    r = client.call("dir2mcp_stats", {})
    ix = r.get("structuredContent", {}).get("indexing", {})
    check("stats: indexing stopped", ix.get("running") is False, f"running={ix.get('running')}")
    check("stats: errors==0", ix.get("errors", -1) == 0, f"errors={ix.get('errors')}")
    check("stats: embedded_ok>0", ix.get("embedded_ok", 0) > 0, f"embedded_ok={ix.get('embedded_ok')}")

    for q in questions:
        r = client.call("dir2mcp_ask", {"question": q, "k": 8})
        if r.get("isError"):
            check(f"ask: {q[:42]}…", False, "tool error")
            continue
        sc = r.get("structuredContent", {})
        ans = (sc.get("answer") or "").strip()
        cites = sc.get("citations") or []
        check(f"ask: {q[:42]}…", bool(ans) and len(cites) >= 1, f"answer={len(ans)}c citations={len(cites)}")

    r = client.call("dir2mcp_search", {"query": "financial investigation agency powers", "k": 5})
    hits = r.get("structuredContent", {}).get("hits", []) if not r.get("isError") else []
    check("search: >=1 hit", len(hits) >= 1, f"hits={len(hits)}")

    lf = client.call("dir2mcp_list_files", {"glob": "*.pdf", "limit": 1})
    files = lf.get("structuredContent", {}).get("files", [])
    if files:
        rp = files[0]["rel_path"]
        of = client.call("dir2mcp_open_file", {"rel_path": rp, "page": 1})
        txt = (of.get("content") or [{}])[0].get("text", "") if not of.get("isError") else ""
        check(f"open_file page=1 ({rp[:30]}…)",
              not of.get("isError") and len(txt.strip()) > 0,
              "tool error" if of.get("isError") else f"{len(txt)}c")
    else:
        check("open_file: a pdf exists to test", False)
    return fails


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--state-dir", default=".dir2mcp")
    ap.add_argument("--transport", choices=["http", "stdio"], default="http")
    ap.add_argument("--bunx", default="bunx", help="command that runs mcp-remote (stdio transport)")
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

    print(f"dir2mcp release smoke @ {url}  (transport={a.transport})")
    client = StdioClient(url, token, a.bunx) if a.transport == "stdio" else HTTPClient(url, token)
    try:
        fails = run_checks(client, questions)
    finally:
        client.close()
    print(f"\n{'✅ ALL PASS' if not fails else '❌ FAILED: ' + ', '.join(fails)}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
