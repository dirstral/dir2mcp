"""Minimal MCP streamable-HTTP client, ported from /mnt/data/rfe-val/measure.py.

The bearer token is read from a file whose path the caller supplies. The token
is never logged, never written into any result, and never a default value.
"""
import json
import os
import time
import urllib.request

PROTO = "2025-11-25"


def read_token(token_file):
    if not token_file:
        raise SystemExit("no token file: pass --token-file or set RFE_RIG_TOKEN_FILE")
    with open(os.path.expanduser(token_file), encoding="utf-8") as fh:
        tok = fh.read().strip()
    if not tok:
        raise SystemExit(f"token file {token_file} is empty")
    return tok


class MCP:
    def __init__(self, url, token, client_name="rfe-rig"):
        self.url = url
        self._token = token
        self.sid = None
        self.n = 0
        self.client_name = client_name

    def _post(self, body, timeout=600):
        h = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "Authorization": f"Bearer {self._token}",
            "MCP-Protocol-Version": PROTO,
        }
        if self.sid:
            h["MCP-Session-Id"] = self.sid
        req = urllib.request.Request(self.url, json.dumps(body).encode(), h)
        with urllib.request.urlopen(req, timeout=timeout) as r:
            sid = r.headers.get("MCP-Session-Id")
            if sid:
                self.sid = sid
            raw = r.read().decode()
        return parse_body(raw)

    def call(self, method, params=None, timeout=600):
        self.n += 1
        body = {"jsonrpc": "2.0", "id": self.n, "method": method}
        if params is not None:
            body["params"] = params
        return self._post(body, timeout)

    def connect(self):
        self.call("initialize", {
            "protocolVersion": PROTO,
            "capabilities": {},
            "clientInfo": {"name": self.client_name, "version": "1"},
        })
        self._post({"jsonrpc": "2.0", "method": "notifications/initialized"})
        return self

    def ask(self, question, timeout=600):
        """Return (answer_text, seconds, error_or_None, structured_content)."""
        t0 = time.time()
        r = self.call("tools/call",
                      {"name": "dir2mcp_ask", "arguments": {"question": question}},
                      timeout)
        dt = time.time() - t0
        res = r.get("result") or {}
        sc = res.get("structuredContent") or {}
        answer = sc.get("answer") or ""
        if not answer:
            for c in res.get("content") or []:
                if c.get("type") == "text":
                    answer = c["text"]
                    break
        return answer, dt, r.get("error"), sc


def parse_body(raw):
    """JSON-RPC body from either a plain JSON response or an SSE frame."""
    if raw.startswith("event:") or raw.startswith("data:"):
        for line in raw.splitlines():
            if line.startswith("data:"):
                raw = line[5:].strip()
                break
    return json.loads(raw) if raw.strip() else {}
