"""The served recognition backend (design 0004 §5).

dir2mcp with `recognize.provider: serve` POSTs `{"path": "<abs media path>"}`
to `/recognize` and indexes the returned annotations. `/health` answers 200
for probes (docling-serve parity). Stdlib-only.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from .emit import build_response
from .model import Document
from .pipeline import Pipeline

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
                media = Path(payload["path"])
            except (ValueError, KeyError):
                self._reply(400, {"error": "body must be JSON with a 'path' field"})
                return
            if not media.is_file():
                self._reply(404, {"error": "media file not found"})
                return
            try:
                annotations = pipeline.annotations_for(media)
            except Exception as exc:  # surface as 502, never a hung request
                self._reply(502, {"error": f"recognition failed: {exc}"})
                return
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
