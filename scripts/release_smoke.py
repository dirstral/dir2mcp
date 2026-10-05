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

The gate checks, and what PASS, FAIL and SKIPPED mean, are in GATE_HELP below
(also printed by --help).

Usage:
  release_smoke.py [--state-dir DIR] [--transport http|stdio] [--bunx CMD] [--question Q ...]
"""
import argparse, json, os, re, select, subprocess, sys, urllib.request

GATE_HELP = """\
Gate (any FAIL -> exit 1):
  * stats: indexing stopped, errors==0, embedded_ok>0
  * ask (each question): non-empty answer AND >=1 citation AND no abstention,
    no tool error. Every question must be one the corpus can answer: an answer
    that abstains fails even when it carries citations. Abstention is
    evidence=insufficient, faithfulness=unsupported, a server refusal text, or
    a "no information" / "does not contain" form in the opening of the answer.
  * search: >=1 hit
  * open_file(page=1) on an extracted PDF (list_files status "ok"): text that
    is not binary and not PDF syntax (%PDF- header, endobj, endstream, xref).
    PDFs that exist but none of which were extracted: FAIL.
    No PDF in the corpus: SKIPPED.

SKIPPED is never PASS. A skip alone does not fail the gate (exit 0), but the
summary then reads "PASS with N SKIPPED", and "ALL PASS" only when nothing
was skipped. Use a corpus with a PDF to run the full gate before a release.
"""

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


# PDF file syntax (#1101). Uncompressed PDF source is mostly letters (obj,
# endobj, stream, BT, Tj and the dictionary keys), so the letter ratio in
# is_texty accepted it as extracted text: the v0.11.4 candidate gate printed
# ALL PASS on 658 characters of raw source from a skipped PDF (#1100). These
# markers are anchored to the shape they have in a PDF (the header at the
# start, a keyword on its own line or after an object body), so prose that
# names the words in a sentence still passes.
_PDF_SYNTAX_RES = (
    re.compile(r"\A\s*%PDF-"),                       # file header
    re.compile(r"(?m)(^|[>\]\s])endobj\s*$"),         # object end
    re.compile(r"(?m)(^|[>\]\s])endstream\s*$"),      # stream end
    re.compile(r"(?m)^\s*xref\s*$"),                  # cross-reference table
    re.compile(r"(?m)^\s*startxref\s*$"),             # pointer to the table
)


def is_pdf_source(s):
    """Return True when s holds PDF file syntax rather than extracted text:
    the %PDF- header at the start, or an endobj, endstream, xref or startxref
    marker in the position it has in a PDF file."""
    s = s or ""
    return any(r.search(s) for r in _PDF_SYNTAX_RES)


def is_texty(s):
    """Heuristic: real extracted text (not raw bytes, PDF syntax or empty).
    Legal prose is mostly letters; binary payloads are not. PDF syntax is
    mostly letters too, so it is refused by is_pdf_source first."""
    s = (s or "").strip()
    if len(s) < 20 or is_pdf_source(s):
        return False
    letters = sum(c.isalpha() or c.isspace() for c in s)
    return letters >= 0.6 * len(s)


# Abstention detection (#1101). An answer that says the corpus has no
# information can still carry citations, so "answer + >=1 citation" passed an
# abstention. Three sources of evidence, any one of which fails the question:
#
# 1. The structured verdicts of the ask result: evidence="insufficient" is the
#    structured form of abstention (SPEC 9.4.3), faithfulness="unsupported"
#    means the answer was withheld (SPEC 9.4.4).
# 2. The fixed refusal texts the server writes in place of an answer
#    (internal/retrieval: insufficientEvidenceAnswer in evidence.go,
#    unfaithfulAnswer in faithfulness.go, the zero-hit fallback in
#    service.go). Matched anywhere in the answer.
# 3. The model's own "no information" wording. The default RAG prompt says
#    "Answer the question using only the provided context." and names no
#    refusal phrase, so the model words an abstention freely. These patterns
#    cover the common forms and are matched only in the opening of the answer,
#    where an abstention puts them, so a grounded answer that states a limit
#    later ("the act gives no information about penalties") still passes.
_SERVER_REFUSALS = (
    "insufficient evidence to answer",
    "i could not verify the answer against the retrieved passages",
    "no relevant context found in the indexed corpus",
)
_ABSTAIN_OPENING_CHARS = 300
_SOURCE_WORDS = r"(?:context|documents?|passages?|sources?|corpus|materials?|texts?|excerpts?)"
_MODEL_ABSTAIN_RES = (
    re.compile(r"\b(?:no|not any|isn't any|is not any) (?:relevant |specific )?information\b"),
    re.compile(_SOURCE_WORDS + r"\b[^.]{0,40}?\b(?:does|do|did)(?: not|n't) "
               r"(?:contain|provide|include|mention|specify|address|cover)\b"),
    re.compile(r"\b(?:cannot|can't|can not|unable to) (?:answer|determine|find)\b"),
    re.compile(r"\bnot (?:mentioned|found|covered|addressed) in the " + _SOURCE_WORDS),
)


def abstention_reason(sc):
    """Return why the ask result sc is an abstention, or None for an answer.
    sc is the ask tool's structuredContent; see the comment above for the
    three signals read."""
    if sc.get("evidence") == "insufficient":
        return "evidence=insufficient"
    if sc.get("faithfulness") == "unsupported":
        return "faithfulness=unsupported (answer withheld)"
    ans = " ".join((sc.get("answer") or "").lower().split())
    for phrase in _SERVER_REFUSALS:
        if phrase in ans:
            return f"server refusal {phrase!r}"
    opening = ans[:_ABSTAIN_OPENING_CHARS]
    for r in _MODEL_ABSTAIN_RES:
        m = r.search(opening)
        if m:
            return f"answer says {m.group(0)!r}"
    return None


def ask_verdict(sc):
    """Return (ok, detail) for one ask result sc (the structuredContent).
    ok needs a non-empty answer, at least one citation and no abstention:
    every gate question must be one the corpus can answer."""
    ans = (sc.get("answer") or "").strip()
    cites = sc.get("citations") or []
    detail = f"answer={len(ans)}c citations={len(cites)}"
    if not ans or not cites:
        return False, detail
    why = abstention_reason(sc)
    if why:
        return False, f"{detail}, abstained: {why}"
    return True, detail


def open_file_verdict(list_result, open_page, max_probe=12):
    """Return (status, detail) for the open_file check, status one of PASS,
    FAIL or SKIPPED.

    list_result is the dir2mcp_list_files result for "**/*.pdf". open_page
    takes a rel_path and returns the page-1 text, or None on a tool error or a
    schema-nonconforming result.

    * list_files failed: FAIL (an absent list is not an empty corpus).
    * no PDF in the corpus: SKIPPED, never PASS.
    * PDFs, but none with status "ok" (extracted and retrievable now, SPEC
      15.5): FAIL, and open_file is not called. A skipped PDF can return its
      raw source (#1100), which proves nothing about extraction.
    * otherwise probe up to max_probe extracted PDFs and PASS on the first
      page that is_texty accepts. A single image-only cover page must not
      fail the gate, so one good page is enough.
    """
    if list_result.get("isError"):
        return "FAIL", "list_files tool error"
    rows = (list_result.get("structuredContent") or {}).get("files") or []
    rows = [f for f in rows if not f.get("deleted")]
    if not rows:
        return "SKIPPED", "the corpus has no PDF to open"
    extracted = [f["rel_path"] for f in rows if f.get("status") == "ok"]
    if not extracted:
        statuses = sorted({str(f.get("status")) for f in rows})
        return "FAIL", f"{len(rows)} pdfs, none extracted (status {', '.join(statuses)})"
    last = ""
    for rp in extracted[:max_probe]:
        txt = open_page(rp)
        if txt is None:
            last = "tool error"
            continue
        if is_texty(txt):
            return "PASS", f"{rp[:30]}… {len(txt.strip())}c"
        last = f"{len(txt.strip())}c " + ("PDF syntax, not extracted text" if is_pdf_source(txt) else "not text-like")
    return "FAIL", f"none of {min(len(extracted), max_probe)} extracted pdfs ({last})"


def summary_line(fails, skips):
    """Return the final summary line. "ALL PASS" only when nothing failed and
    nothing was skipped; a skip gives "PASS with N SKIPPED", so a skip can
    never read as a full pass."""
    skipped = f"{len(skips)} SKIPPED: {', '.join(skips)}" if skips else ""
    if fails:
        return "❌ FAILED: " + ", ".join(fails) + (f" ({skipped})" if skipped else "")
    if skips:
        return f"⚠️  PASS with {skipped}"
    return "✅ ALL PASS"


def _resolve_ref(ref, root):
    # Fail closed: a typo'd ref ('#/definitions/Hti') must surface as an error,
    # not silently resolve to {} (which would validate as success and skip the
    # nested checks). Returns None when the ref is malformed or unresolvable.
    if not isinstance(ref, str) or not ref.startswith("#/"):
        return None
    node = root
    for part in ref.lstrip("#/").split("/"):
        if not isinstance(node, dict) or part not in node:
            return None
        node = node[part]
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
        target = _resolve_ref(schema["$ref"], root)
        if target is None:
            return [f"{path}: unresolved $ref {schema['$ref']!r}"]
        return schema_errors(target, inst, root, path)
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
    # bool is a subclass of int in Python, so guard integer/number explicitly —
    # else True/False would pass where strict JSON Schema rejects them.
    if t == "string" and not isinstance(inst, str):
        return [f"{path}: expected string"]
    if t == "integer" and (not isinstance(inst, int) or isinstance(inst, bool)):
        return [f"{path}: expected integer"]
    if t == "number" and (not isinstance(inst, (int, float)) or isinstance(inst, bool)):
        return [f"{path}: expected number"]
    if t == "boolean" and not isinstance(inst, bool):
        return [f"{path}: expected boolean"]
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
        # bs-005: complete the handshake before any tool traffic. The server
        # confirms the accepted notification with HTTP 202 and now rejects
        # tools/* on a session that skipped it.
        _, _, status = self._post_raw({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})
        if status != 202:
            raise RuntimeError(f"http: notifications/initialized failed with HTTP {status}")

    def _post(self, body):
        out, sid, _ = self._post_raw(body)
        return out, sid

    def _post_raw(self, body):
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
        return out, resp.headers.get("Mcp-Session-Id"), resp.status

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
    """Run every gate check against client and return (fails, skips): the
    names of the failed checks and of the skipped ones."""
    fails, skips = [], []
    def report(name, status, detail=""):
        print(f"  [{status}] {name}{(' — ' + detail) if detail else ''}")
        if status == "FAIL":
            fails.append(name)
        elif status == "SKIPPED":
            skips.append(name)

    def check(name, ok, detail=""):
        report(name, "PASS" if ok else "FAIL", detail)

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

    # Returns True when the response is safe to read downstream — either the tool
    # declares no outputSchema (so strict clients don't validate it either; stats
    # is one) or its structuredContent conforms. Returns False (and records a
    # FAIL) when a declared schema is present but the content is missing or
    # non-conforming, so callers can skip interpreting an already-rejected result.
    def validate(tool, r):
        sch = schemas.get(tool)
        if not sch:
            return True
        sc = r.get("structuredContent")
        if sc is None:
            check(f"{tool}: structuredContent present (schema declared)", False, "missing structuredContent")
            return False
        errs = schema_errors(sch, sc, sch)
        check(f"{tool}: structuredContent conforms to outputSchema",
              not errs, "ok" if not errs else f"{len(errs)} err — {errs[0][:90]}")
        return not errs

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
        if not validate("dir2mcp_ask", r):
            continue  # schema already failed; don't read fields off a rejected result
        ok, detail = ask_verdict(r.get("structuredContent") or {})
        check(f"ask: {q[:42]}…", ok, detail)

    r = client.call("dir2mcp_search", {"query": "financial investigation agency powers", "k": 5})
    validate("dir2mcp_search", r)
    hits = r.get("structuredContent", {}).get("hits", []) if not r.get("isError") else []
    check("search: >=1 hit", len(hits) >= 1, f"hits={len(hits)}")

    # `*` does not cross `/` (canonical glob dialect), so "*.pdf" saw only PDFs at
    # the corpus root and failed a corpus that keeps them in a subfolder.
    # "**/" also matches zero directories, so root-level PDFs still match.
    # The limit is wider than the probe count (12) so a corpus with many
    # skipped PDFs still offers extracted ones to probe.
    lf = client.call("dir2mcp_list_files", {"glob": "**/*.pdf", "limit": 100})
    if not lf.get("isError") and not validate("dir2mcp_list_files", lf):
        return fails, skips  # schema failed; the files list can't be trusted to drive open_file

    def open_page(rp):
        """Return the page-1 text of rp, or None on a tool error or a
        schema-nonconforming result (a rejected result is not a text page)."""
        of = client.call("dir2mcp_open_file", {"rel_path": rp, "page": 1})
        if of.get("isError") or not validate("dir2mcp_open_file", of):
            return None
        return (of.get("content") or [{}])[0].get("text", "")

    status, detail = open_file_verdict(lf, open_page)
    report("open_file page=1 returns text", status, detail)
    return fails, skips


def main():
    """Parse the flags, connect to the daemon, run the gate and exit 1 on any
    FAIL (a SKIPPED check alone exits 0)."""
    ap = argparse.ArgumentParser(
        description=__doc__.split("\n\n", 1)[0],
        epilog=GATE_HELP,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--state-dir", default=".dir2mcp")
    ap.add_argument("--transport", choices=["http", "stdio"], default="http")
    ap.add_argument("--bunx", default="bunx", help="command that runs mcp-remote (stdio transport)")
    ap.add_argument("--question", action="append", default=[],
                    help="a question the corpus CAN answer (repeatable); an abstaining answer fails it")
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
        fails, skips = run_checks(client, questions)
    finally:
        client.close()
    print(f"\n{summary_line(fails, skips)}")
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
