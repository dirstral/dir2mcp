"""The served recognition backend (design 0004 §5).

dir2mcp with `recognize.provider: serve` POSTs `{"path": "<abs media path>"}`
to `/recognize` and indexes the returned annotations. `/health` answers 200
for probes (docling-serve parity). Stdlib-only.

One pipeline serves every request, so the recognizers it caches are shared by
every request too, and this runs on ThreadingHTTPServer. The pipeline owns that
contract: it builds each recognizer once and lets one request at a time into it.
See `pipeline._Shared`.
"""

from __future__ import annotations

import json
import logging
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from .emit import build_response
from .model import Document
from .pipeline import Pipeline
from .recognizers.base import scrubbed_traceback

log = logging.getLogger(__name__)

MAX_REQUEST_BYTES = 1 << 20


def make_handler(pipeline: Pipeline):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802 (http.server API)
            if self.path == "/health":
                self._reply(200, {"status": "ok"})
            else:
                self._reply(404, {"error": "not found"})

        def do_POST(self):  # noqa: N802
            if self.path != "/recognize":
                self._reply(404, {"error": "not found"})
                return
            try:
                declared = int(self.headers.get("Content-Length", 0))
                if declared < 0:
                    raise ValueError("negative Content-Length")
                length = min(declared, MAX_REQUEST_BYTES)
                payload = json.loads(self.rfile.read(length) or b"{}")
                # TypeError covers a valid-JSON but non-object body (null, [], a
                # number): payload["path"] would otherwise raise uncaught.
                media = Path(payload["path"])
            except (ValueError, KeyError, TypeError):
                self._reply(400, {"error": "body must be JSON with a 'path' field"})
                return
            if not media.is_file():
                self._reply(404, {"error": "media file not found"})
                return
            try:
                annotations = pipeline.annotations_for(media)
            except Exception as exc:  # surface as 502, never a hung request
                # The body carries a STABLE code and no exception text: the
                # message can interpolate a request URL, a header or a local
                # path (CWE-209), and the caller (dir2mcp) deliberately does
                # not echo backend bodies anyway. The reason lives in this
                # process's journal, frames intact and message scrubbed, so a
                # failed run is diagnosable here and leaks nothing there (#945).
                log.error("recognize %s failed\n%s", media.name, scrubbed_traceback(exc))
                self._reply(502, {"error": "recognition failed", "code": "RECOGNITION_FAILED"})
                return
            skipped = pipeline.skipped
            if skipped:
                # A degraded request is a 200 with fewer cues. That is the right
                # wire contract (the schema is closed and the caller cannot act
                # on it), but it must not be a silent one on this side.
                log.warning("recognize %s: %d recognizer(s) skipped: %s",
                            media.name, len(skipped), "; ".join(skipped))
            doc = Document(media=media.name, annotations=annotations)
            self._reply(200, build_response(doc, pipeline.roster))

        def _reply(self, status: int, payload: dict) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, fmt, *args):  # quiet by default; dir2mcp logs its side
            pass

    return Handler


def serve(pipeline: Pipeline, host: str = "127.0.0.1", port: int = 8765) -> ThreadingHTTPServer:
    """Build the server (caller runs serve_forever; tests use ephemeral ports)."""
    return ThreadingHTTPServer((host, port), make_handler(pipeline))
