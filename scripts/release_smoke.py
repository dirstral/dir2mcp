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


def _result_or_error(msg):
    """Map a JSON-RPC response message to a tool result, surfacing protocol
    errors as an isError result instead of silently collapsing them to {} (so a
    handshake/auth failure produces a clear failure, not misleading downstream
    FAILs)."""
    if msg is None:
        return {"isError": True, "content": [{"type": "text", "text": "no response (Failed to call tool)"}]}
    if msg.get("error"):
        e = msg["error"]
        return {"isError": True, "content": [{"type": "text", "text": f"JSON-RPC error {e.get('code')}: {e.get('message')}"}]}
    return msg.get("result", {})


def _check_init(msg, transport):
    """Fail fast with the real protocol/auth error from an initialize response."""
    if msg is None:
        raise RuntimeError(f"{transport}: no response to initialize (server/bridge unreachable?)")
    if msg.get("error"):
        e = msg["error"]
        raise RuntimeError(f"{transport}: initialize failed — JSON-RPC error {e.get('code')}: {e.get('message')}")


def is_texty(s):
    """Heuristic: real extracted text (not raw bytes / empty). Legal prose is
    mostly letters; binary payloads are not."""
    s = (s or "").strip()
    if len(s) < 20:
        return False
    letters = sum(c.isalpha() or c.isspace() for c in s)
    return letters >= 0.6 * len(s)


def _resolve_ref(ref, root):
    node = root
    for part in ref.lstrip("#/").split("/"):
        if not isinstance(node, dict):
            return {}
        node = node.get(part, {})
    return node


def schema_errors(schema, inst, root, path="$"):
    """Minimal JSON-Schema check for the subset the dir2mcp outputSchemas use
    ($ref/oneOf/const/enum/object+additionalProperties+required/array/scalars).
    Mirrors what a strict MCP client (Claude Desktop) does to structuredContent —
    the check that would have caught #387 (a serialized field not declared in an
    additionalProperties:false object). NOT a full validator; deliberately small
    and dependency-free."""
    if not isinstance(schema, dict):
        return []
    if "$ref" in schema:
        return schema_errors(_resolve_ref(schema["$ref"], root), inst, root, path)
    if "oneOf" in schema:
        matches = [s for s in schema["oneOf"] if not schema_errors(s, inst, root, path)]
        return [] if len(matches) == 1 else [f"{path}: matched {len(matches)} oneOf branches (want 1)"]
    if "const" in schema:
        return [] if inst == schema["const"] else [f"{path}: {inst!r} != const {schema['const']!r}"]
    if "enum" in schema:
        return [] if inst in schema["enum"] else [f"{path}: {inst!r} not in enum"]
    t = schema.get("type")
    if t == "object" or (t is None and "properties" in schema):
        if not isinstance(inst, dict):
            return [f"{path}: expected object"]
        errs, props = [], schema.get("properties", {})
        for req in schema.get("required", []):
            if req not in inst:
                errs.append(f"{path}.{req}: required property missing")
        addl = schema.get("additionalProperties", True)
        for k, v in inst.items():
            if k in props:
                errs += schema_errors(props[k], v, root, f"{path}.{k}")
            elif addl is False:
                errs.append(f"{path}.{k}: additional property not allowed by schema")
            elif isinstance(addl, dict):
                errs += schema_errors(addl, v, root, f"{path}.{k}")
        return errs
    if t == "array":
        if not isinstance(inst, list):
            return [f"{path}: expected array"]
        items, errs = schema.get("items"), []
        if items:
            for i, v in enumerate(inst):
                errs += schema_errors(items, v, root, f"{path}[{i}]")
        return errs
    scal = {"string": str, "integer": int, "number": (int, float), "boolean": bool}
    if t in scal and not isinstance(inst, scal[t]):
        return [f"{path}: expected {t}"]
    return []


class HTTPClient:
    """MCP over streamable-HTTP, directly to the daemon."""

    def __init__(self, url, token):
        self.url, self.token, self.sid = url, token, None
        msg, sid = self._post({"jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": PROTO, "capabilities": {},
                       "clientInfo": {"name": "release-smoke", "version": "1"}}})
        _check_init(msg, "http")
        self.sid = sid

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
        return _result_or_error(d)

    def list_tools(self):
        d, _ = self._post({"jsonrpc": "2.0", "id": 50, "method": "tools/list", "params": {}})
        res = _result_or_error(d)
        return {t["name"]: t.get("outputSchema") for t in (res.get("tools") or [])}

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
        _check_init(self._read_until(1, timeout=90), "stdio")
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
        # A None here is the exact "Failed to call tool" class — the bridge never
        # returned a result; _result_or_error surfaces it as a tool error.
        return _result_or_error(self._read_until(rid, timeout=120))

    def list_tools(self):
        self._id += 1
        rid = self._id
        self._send({"jsonrpc": "2.0", "id": rid, "method": "tools/list", "params": {}})
        res = _result_or_error(self._read_until(rid, timeout=60))
        return {t["name"]: t.get("outputSchema") for t in (res.get("tools") or [])}

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

    # Fetch each tool's declared outputSchema once. Strict MCP clients (Claude
    # Desktop) validate structuredContent against it and reject the whole call on
    # any mismatch — see #387, where a serialized hit field (modality) absent from
    # an additionalProperties:false schema made search/ask fail with "Failed to
    # call tool" while curl and this gate (which formerly didn't validate) passed.
    try:
        schemas = client.list_tools()
    except Exception as e:
        schemas = {}
        check("tools/list (for schema validation)", False, str(e)[:80])

    def validate(tool, r):
        sch, sc = schemas.get(tool), r.get("structuredContent")
        if not sch or sc is None:
            return
        errs = schema_errors(sch, sc, sch)
        check(f"{tool}: structuredContent conforms to outputSchema",
              not errs, "ok" if not errs else f"{len(errs)} err — {errs[0][:90]}")

    r = client.call("dir2mcp_stats", {})
    validate("dir2mcp_stats", r)
    ix = r.get("structuredContent", {}).get("indexing", {})
    check("stats: indexing stopped", ix.get("running") is False, f"running={ix.get('running')}")
    check("stats: errors==0", ix.get("errors", -1) == 0, f"errors={ix.get('errors')}")
    check("stats: embedded_ok>0", ix.get("embedded_ok", 0) > 0, f"embedded_ok={ix.get('embedded_ok')}")

    for q in questions:
        r = client.call("dir2mcp_ask", {"question": q, "k": 8})
        if r.get("isError"):
            check(f"ask: {q[:42]}…", False, "tool error")
            continue
        validate("dir2mcp_ask", r)
        sc = r.get("structuredContent", {})
        ans = (sc.get("answer") or "").strip()
        cites = sc.get("citations") or []
        check(f"ask: {q[:42]}…", bool(ans) and len(cites) >= 1, f"answer={len(ans)}c citations={len(cites)}")

    r = client.call("dir2mcp_search", {"query": "financial investigation agency powers", "k": 5})
    validate("dir2mcp_search", r)
    hits = r.get("structuredContent", {}).get("hits", []) if not r.get("isError") else []
    check("search: >=1 hit", len(hits) >= 1, f"hits={len(hits)}")

    lf = client.call("dir2mcp_list_files", {"glob": "*.pdf", "limit": 12})
    validate("dir2mcp_list_files", lf)
    files = [f["rel_path"] for f in lf.get("structuredContent", {}).get("files", [])]
    if not files:
        check("open_file: a pdf exists to test", False)
        return fails
    # Probe several PDFs rather than whichever sorts first: a single image-only
    # cover page must not fail the gate, and the result must be real text (not raw
    # bytes). Pass on the first page that yields printable text; fail only if none
    # of the sampled PDFs do (which would mean open_file page reads are broken).
    opened, last = None, ""
    for rp in files:
        of = client.call("dir2mcp_open_file", {"rel_path": rp, "page": 1})
        if of.get("isError"):
            last = "tool error"
            continue
        validate("dir2mcp_open_file", of)
        txt = (of.get("content") or [{}])[0].get("text", "")
        if is_texty(txt):
            opened = (rp, len(txt.strip()))
            break
        last = f"{len(txt.strip())}c not text-like"
    check("open_file page=1 returns text",
          opened is not None,
          f"{opened[0][:30]}… {opened[1]}c" if opened else f"none of {len(files)} pdfs ({last})")
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
