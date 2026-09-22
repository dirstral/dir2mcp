"""Decoders that turn a recording into an rfe_rig transcript.

A decoder is named by a spec string:

  http://HOST:PORT?model=NAME[&name=LABEL][&words=1]
      An OpenAI-compatible POST /v1/audio/transcriptions server returning
      verbose_json. The audio is converted to 16 kHz mono WAV first, which
      every server here accepts and GigaAM requires. Version comes from
      GET /health.
  sqlite:STATE_DIR
      The transcript dir2mcp already delivered for the recording, read from
      the state sqlite (live chunks and their word timings). No decode runs.
      Version is the representation's model and rep_hash.
  mms:ADAPTER
      facebook/mms-1b-all on CPU through mms_decode.py, run under the
      interpreter given by --mms-python (it needs torch and transformers).
      Version is the adapter, the cached model revision and the library
      versions, read with `mms_decode.py --version-only` before decoding.
  fw:MODEL_DIR[?language=xx&name=LABEL]
      A local CTranslate2 whisper model on CPU through fw_decode.py, run under
      the interpreter given by --fw-python (it needs faster-whisper).

The cache key is (recording, decoder name, decoder version). The version is
resolved before the cache is consulted, so a new model revision or a re-indexed
state is a miss and the same decoder is a hit.
"""
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
import uuid

from . import coverage, transcripts

HERE = os.path.dirname(os.path.abspath(__file__))


class DecoderError(RuntimeError):
    pass


def parse_spec(spec):
    """Return (kind, target, params) for a decoder spec string."""
    if spec.startswith(("http://", "https://")):
        u = urllib.parse.urlsplit(spec)
        params = {k: v[-1] for k, v in urllib.parse.parse_qs(u.query).items()}
        base = urllib.parse.urlunsplit((u.scheme, u.netloc, u.path.rstrip("/"), "", ""))
        return "http", base, params
    for kind in ("sqlite", "mms", "fw"):
        if spec.startswith(kind + ":"):
            rest = spec[len(kind) + 1:]
            target, _, query = rest.partition("?")
            params = {k: v[-1] for k, v in urllib.parse.parse_qs(query).items()}
            return kind, target, params
    raise DecoderError(f"unknown decoder spec: {spec!r}")


def to_wav16k(src, dst):
    """16 kHz mono 16-bit WAV via ffmpeg. Whisper resamples to this anyway."""
    subprocess.run(
        ["ffmpeg", "-v", "error", "-y", "-i", src, "-ac", "1", "-ar", "16000",
         "-c:a", "pcm_s16le", dst],
        check=True, capture_output=True, timeout=1800,
    )
    return dst


# ---------------------------------------------------------------- http


def http_health(base):
    try:
        with urllib.request.urlopen(base + "/health", timeout=10) as r:
            return json.loads(r.read().decode() or "{}")
    except (OSError, ValueError) as e:
        raise DecoderError(f"{base}/health failed: {e}") from e


def _multipart(fields, file_field, filename, payload):
    boundary = "----rferig" + uuid.uuid4().hex
    body = bytearray()
    for k, v in fields:
        body += f"--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n".encode()
    body += (f"--{boundary}\r\nContent-Disposition: form-data; name=\"{file_field}\";"
             f" filename=\"{filename}\"\r\nContent-Type: audio/wav\r\n\r\n").encode()
    body += payload
    body += f"\r\n--{boundary}--\r\n".encode()
    return f"multipart/form-data; boundary={boundary}", bytes(body)


def http_transcribe(base, model, wav_path, want_words=True, timeout=7200):
    fields = [("model", model), ("response_format", "verbose_json"), ("task", "transcribe")]
    if want_words:
        fields += [("timestamp_granularities", "word"), ("timestamp_granularities[]", "word")]
    with open(wav_path, "rb") as fh:
        payload = fh.read()
    ctype, body = _multipart(fields, "file", os.path.basename(wav_path), payload)
    req = urllib.request.Request(base + "/v1/audio/transcriptions", body,
                                 {"Content-Type": ctype})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def _segments_from_openai(resp):
    segs = []
    words = resp.get("words") or []
    for s in resp.get("segments") or []:
        seg = {"start": s["start"], "end": s["end"], "text": s.get("text", "")}
        ws = s.get("words")
        if not ws and words:
            ws = [w for w in words if s["start"] <= float(w["start"]) < s["end"]]
        if ws:
            seg["words"] = [{"start": w["start"], "end": w["end"], "word": w["word"]} for w in ws]
        segs.append(seg)
    return segs


# ---------------------------------------------------------------- helpers in another interpreter


def _run_helper(python, script, args, log):
    cmd = [python, os.path.join(HERE, script)] + args
    log("  " + " ".join(cmd))
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr[-4000:])
        raise DecoderError(f"{script} exited {proc.returncode}")
    if proc.stderr.strip():
        for line in proc.stderr.strip().splitlines()[-5:]:
            log("  helper: " + line)
    return json.loads(proc.stdout)


# ---------------------------------------------------------------- identity


def model_dir_fingerprint(model_dir):
    """Short digest of a local model directory: every file's relative name,
    size and modification time. Two directories with the same basename but
    different weights, or one whose weights were replaced in place, get
    different fingerprints, so they never share a cache entry. A missing
    directory is "nodir"; the decode then fails on its own."""
    if not os.path.isdir(model_dir):
        return "nodir"
    h = hashlib.sha1()
    for root, dirs, files in os.walk(model_dir):
        dirs.sort()
        for name in sorted(files):
            p = os.path.join(root, name)
            try:
                st = os.stat(p)
            except OSError:
                continue
            rel = os.path.relpath(p, model_dir)
            h.update(f"{rel}\0{st.st_size}\0{st.st_mtime_ns}\n".encode("utf-8"))
    return h.hexdigest()[:12]


class Decoder:
    """A resolved decoder: kind, target, params, name, version and a
    `decode(recording, duration_s, log)` that returns segments and language."""

    def __init__(self, spec, mms_python="python3", fw_python="python3", threads=8,
                 mms_chunk_s=30.0, log=print):
        self.spec = spec
        self.kind, self.target, self.params = parse_spec(spec)
        self.mms_python = mms_python
        self.fw_python = fw_python
        self.threads = threads
        self.mms_chunk_s = mms_chunk_s
        self.log = log
        self.model = ""
        self.detail = {}
        self.name, self.version = self._identify()

    # -- identity -----------------------------------------------------------

    def _identify(self):
        if self.kind == "http":
            model = self.params.get("model", "")
            health = http_health(self.target)
            if health.get("status") != "ok":
                raise DecoderError(f"{self.target}/health not ok: {health}")
            self.model = model
            self.detail = {"url": self.target, "health": health}
            name = self.params.get("name") or \
                f"{urllib.parse.urlsplit(self.target).netloc}-{model or 'default'}"
            words = "1" if self.params.get("words", "1") != "0" else "0"
            return name, f"{health.get('model', '')}|{model}|words{words}"
        if self.kind == "sqlite":
            # Identity depends on the recording (its rep_hash); resolved per call.
            return "dir2mcp", ""
        if self.kind == "mms":
            out = _run_helper(self.mms_python, "mms_decode.py",
                              ["--version-only", "--adapter", self.target], self.log)
            self.model = out.get("model", "facebook/mms-1b-all")
            self.detail = {"adapter": self.target, "chunk_s": self.mms_chunk_s,
                           "model_revision": out.get("model_revision"),
                           "transformers": out.get("transformers"), "torch": out.get("torch")}
            name = self.params.get("name") or f"mms-1b-all-{self.target}"
            return name, (f"{self.target}|{(out.get('model_revision') or '')[:12]}"
                          f"|tf{out.get('transformers', '')}|chunk{self.mms_chunk_s:g}")
        if self.kind == "fw":
            out = _run_helper(self.fw_python, "fw_decode.py", ["--version-only"], self.log)
            base = os.path.basename(self.target.rstrip("/"))
            language = self.params.get("language", "")
            self.model = self.target
            self.detail = {"language": language, "faster_whisper": out.get("faster_whisper"),
                           "compute_type": out.get("compute_type")}
            name = self.params.get("name") or f"fw-{base}"
            return name, (f"{base}|{model_dir_fingerprint(self.target)}|{language}"
                          f"|fw{out.get('faster_whisper', '')}")
        raise DecoderError(self.kind)  # pragma: no cover - parse_spec rejects other kinds

    def identity_for(self, recording):
        """(name, version) for one recording; only sqlite depends on it."""
        if self.kind != "sqlite":
            return self.name, self.version
        rel = os.path.basename(recording)
        with tempfile.TemporaryDirectory(prefix="rfe-rig-db-") as tmp:
            db = coverage.snapshot_sqlite(self.target, tmp)
            meta, segs = coverage.transcript_from_db(db, rel)
        if meta is None:
            raise DecoderError(f"{self.target}: no live transcript representation for {rel}")
        self._sqlite_cache = (rel, meta, segs)
        model = meta.get("model") or "unknown"
        self.model = model
        self.detail = {k: meta.get(k) for k in ("provider", "model", "language", "language_source",
                                                "language_confidence", "rep_hash", "coverage")}
        return (f"dir2mcp-{meta.get('provider', 'stt')}-{model}",
                f"{model}|{meta.get('rep_hash', '')[:12]}")

    # -- decoding -----------------------------------------------------------

    def decode(self, recording, duration_s):
        """Return (segments, language, duration_s)."""
        log = self.log
        if self.kind == "sqlite":
            rel, meta, segs = getattr(self, "_sqlite_cache", (None, None, None))
            if rel != os.path.basename(recording):
                self.identity_for(recording)
                rel, meta, segs = self._sqlite_cache
            log(f"  extracted {len(segs)} delivered chunks from the dir2mcp state (no decode)")
            return segs, meta.get("language", ""), duration_s
        with tempfile.TemporaryDirectory(prefix="rfe-rig-wav-") as tmp:
            wav = to_wav16k(recording, os.path.join(tmp, "audio16k.wav"))
            if self.kind == "http":
                log(f"  POST {self.target}/v1/audio/transcriptions"
                    f" model={self.model or '(server default)'}")
                resp = http_transcribe(self.target, self.model, wav,
                                       want_words=self.params.get("words", "1") != "0")
                return (_segments_from_openai(resp), resp.get("language", ""),
                        duration_s or resp.get("duration"))
            if self.kind == "mms":
                out = _run_helper(self.mms_python, "mms_decode.py",
                                  ["--audio", wav, "--adapter", self.target,
                                   "--chunk-s", str(self.mms_chunk_s),
                                   "--threads", str(self.threads)], log)
                return out["segments"], self.target, duration_s
            if self.kind == "fw":
                args = ["--audio", wav, "--model-dir", self.target, "--threads", str(self.threads)]
                language = self.params.get("language", "")
                if language:
                    args += ["--language", language]
                out = _run_helper(self.fw_python, "fw_decode.py", args, log)
                return out["segments"], out.get("language", ""), duration_s
        raise DecoderError(self.kind)  # pragma: no cover


# ---------------------------------------------------------------- entry


def transcribe(spec, recording, results_dir, log=print, force=False, mms_python="python3",
               fw_python="python3", threads=8, mms_chunk_s=30.0, decoder=None):
    """Decode (or load from cache) and return (transcript, path, cached).

    Pass a prepared `decoder` to reuse one identity across many recordings.
    """
    dec = decoder or Decoder(spec, mms_python=mms_python, fw_python=fw_python, threads=threads,
                             mms_chunk_s=mms_chunk_s, log=log)
    name, version = dec.identity_for(recording)
    dec_id = transcripts.decoder_id(name, version)
    path = transcripts.cache_path(results_dir, recording, dec_id)
    if not force and os.path.exists(path):
        log(f"  cache hit {path}")
        return transcripts.load(path), path, True
    log(f"decoding {os.path.basename(recording)} with {spec}")
    duration_s = transcripts.probe_duration_s(recording)
    segments, language, duration_s = dec.decode(recording, duration_s)
    t = transcripts.build(recording, duration_s,
                          {"kind": dec.kind, "name": name, "version": version,
                           "model": dec.model, "detail": dec.detail},
                          segments, language)
    transcripts.save(t, path)
    return t, path, False
