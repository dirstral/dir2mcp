"""Per-recording coverage baseline read from a dir2mcp state sqlite.

The daemon holds the live database, so it is snapshotted into a temporary
directory first with SQLite's online backup API, which yields one coherent
database whatever the daemon writes or checkpoints meanwhile, and the copy is
opened read-only.

Reported per transcript representation:
  language, language_source, language_confidence   from meta_json
  coverage                                          the SPEC 8.6.13 object, or None
  language_covered                                  from meta_json, or None
  chunks_live / chunks_deleted                      chunk rows
  window_s                                          live chunk time-span lengths
  delivered_covered_s                               union of live chunk spans
  delivered_coverage                                that union over the recording
                                                    duration (ffprobe), or None
                                                    when the media is not given

"delivered coverage" is the #566 measure: the share of the audio for which a
chunk survived the quality gate. It is what an editor can search. It is not
the 8.6.13 decode coverage, which says how much audio the decoder attempted.
"""
import json
import os
import sqlite3
import statistics
import tempfile

from .transcripts import probe_duration_s, text_stats

SCHEMA = "rfe_rig.coverage.v1"


def snapshot_sqlite(state_dir, tmp_dir):
    """Snapshot meta.sqlite into tmp_dir as one coherent database.

    A file copy of the database and its -wal sidecar can capture two different
    states when the daemon commits or checkpoints between the copies. The
    backup API reads through one transaction, so the copy is a single point in
    time and includes what the WAL held. Nothing is written to the daemon's
    files.
    """
    src = os.path.join(state_dir, "meta.sqlite")
    if not os.path.exists(src):
        raise SystemExit(f"no meta.sqlite under {state_dir}")
    dst = os.path.join(tmp_dir, "meta.sqlite")
    source = sqlite3.connect(src)
    try:
        target = sqlite3.connect(dst)
        try:
            source.backup(target)
        finally:
            target.close()
    finally:
        source.close()
    return dst


def union_seconds(spans_ms):
    """Length of the union of [start, end) spans, in seconds."""
    total = 0
    cur_s = cur_e = None
    for s, e in sorted(spans_ms):
        if e <= s:
            continue
        if cur_e is None or s > cur_e:
            if cur_e is not None:
                total += cur_e - cur_s
            cur_s, cur_e = s, e
        elif e > cur_e:
            cur_e = e
    if cur_e is not None:
        total += cur_e - cur_s
    return total / 1000.0


def _meta(raw):
    try:
        return json.loads(raw) if raw else {}
    except ValueError:
        return {"_unparseable": True}


def report_from_db(db_path, media_dir=None):
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        return _report(con, media_dir)
    finally:
        con.close()


def _report(con, media_dir):
    out = []
    reps = con.execute(
        "select r.rep_id, d.rel_path, r.rep_type, r.meta_json, r.deleted, d.status, d.skip_reason"
        " from representations r join documents d on d.doc_id=r.doc_id"
        " where r.rep_type='transcript' order by d.rel_path")
    for rep_id, rel_path, rep_type, meta_json, rdeleted, dstatus, skip in reps:
        meta = _meta(meta_json)
        live = list(con.execute(
            "select c.chunk_id, c.text, c.language, sp.start, sp.end, sp.extra_json"
            " from chunks c left join spans sp on sp.chunk_id=c.chunk_id and sp.span_kind='time'"
            " where c.rep_id=? and c.deleted=0 order by sp.start", (rep_id,)))
        deleted = con.execute(
            "select count(*) from chunks where rep_id=? and deleted=1", (rep_id,)).fetchone()[0]
        # The chunks retired by a re-chunking (dir2mcp #955 went from 7.5 s to
        # 34 s windows) are still rows with deleted=1. Their span union is the
        # coverage under the earlier, finer chunking, which is the regime the
        # #566 "27%" was measured in.
        deleted_spans = list(con.execute(
            "select sp.start, sp.end from chunks c join spans sp on sp.chunk_id=c.chunk_id"
            " and sp.span_kind='time' where c.rep_id=? and c.deleted=1", (rep_id,)))
        spans = [(s, e) for _, _, _, s, e, _ in live if s is not None and e is not None]
        lens = [(e - s) / 1000.0 for s, e in spans if e > s]
        words = 0
        for _, _, _, _, _, extra in live:
            if extra:
                try:
                    words += len(json.loads(extra).get("words") or [])
                except ValueError:
                    pass
        langs = {}
        for _, _, lang, _, _, _ in live:
            langs[lang or ""] = langs.get(lang or "", 0) + 1
        duration = None
        if media_dir:
            p = os.path.join(media_dir, rel_path)
            if os.path.exists(p):
                duration = probe_duration_s(p)
        covered = union_seconds(spans)
        covered_deleted = union_seconds(deleted_spans)
        row = {
            "rel_path": rel_path,
            "rep_type": rep_type,
            "document_status": dstatus,
            "skip_reason": skip or "",
            "rep_deleted": bool(rdeleted),
            "provider": meta.get("provider"),
            "model": meta.get("model"),
            "language": meta.get("language"),
            "language_source": meta.get("language_source"),
            "language_confidence": meta.get("language_confidence"),
            "coverage": meta.get("coverage"),
            "language_covered": meta.get("language_covered"),
            "chunks_live": len(live),
            "chunks_deleted": deleted,
            "chunk_languages": langs,
            "window_s": {
                "mean": round(statistics.mean(lens), 1) if lens else None,
                "median": round(statistics.median(lens), 1) if lens else None,
                "min": round(min(lens), 1) if lens else None,
                "max": round(max(lens), 1) if lens else None,
            },
            "words_live": words,
            "chars_live": sum(len(t or "") for _, t, _, _, _, _ in live),
            "delivered_covered_s": round(covered, 1),
            "duration_s": round(duration, 1) if duration else None,
            "delivered_coverage": round(covered / duration, 3) if duration else None,
            "deleted_covered_s": round(covered_deleted, 1),
            "deleted_coverage": round(covered_deleted / duration, 3) if duration else None,
        }
        out.append(row)
    return out


def transcript_from_db(db_path, rel_path):
    """The delivered transcript of one recording as rfe_rig segments.

    Live chunks become segments; the word timings in spans.extra_json become
    the segment's words. Returns (meta, segments) or (None, []) when the
    recording has no transcript representation.
    """
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = con.execute(
            "select r.rep_id, r.meta_json, r.rep_hash from representations r"
            " join documents d on d.doc_id=r.doc_id"
            " where d.rel_path=? and r.rep_type='transcript' and r.deleted=0", (rel_path,)
        ).fetchone()
        if not row:
            return None, []
        rep_id, meta_json, rep_hash = row
        meta = _meta(meta_json)
        meta["rep_hash"] = rep_hash
        segs = []
        for text, s, e, extra in con.execute(
                "select c.text, sp.start, sp.end, sp.extra_json from chunks c"
                " join spans sp on sp.chunk_id=c.chunk_id and sp.span_kind='time'"
                " where c.rep_id=? and c.deleted=0 order by sp.start", (rep_id,)):
            seg = {"start": s / 1000.0, "end": e / 1000.0, "text": text}
            if extra:
                try:
                    ws = json.loads(extra).get("words") or []
                except ValueError:
                    ws = []
                if ws:
                    seg["words"] = [
                        {"start": w["t"] / 1000.0, "end": (w["t"] + w.get("d", 0)) / 1000.0,
                         "word": w.get("w", "")} for w in ws]
            segs.append(seg)
        return meta, segs
    finally:
        con.close()


def run(state_dir, media_dir=None):
    with tempfile.TemporaryDirectory(prefix="rfe-rig-cov-") as tmp:
        db = snapshot_sqlite(state_dir, tmp)
        rows = report_from_db(db, media_dir)
    return {"schema": SCHEMA, "state_dir": state_dir, "media_dir": media_dir,
            "recordings": rows}


def format_table(rep):
    hdr = (f"{'recording':38s} {'lang':5s} {'src':9s} {'conf':>5s} {'8.6.13':>6s} {'lcov':>5s}"
           f" {'live':>5s} {'del':>5s} {'win_mean':>8s} {'win_max':>7s} {'deliv_s':>8s} {'dur_s':>8s} {'deliv%':>6s}"
           f" {'del%':>6s}")
    lines = [hdr]
    for r in rep["recordings"]:
        conf = r["language_confidence"]
        lines.append(
            f"{os.path.basename(r['rel_path']):38s} {str(r['language'] or '-'):5s}"
            f" {str(r['language_source'] or '-'):9s} {('%.2f' % conf) if isinstance(conf, (int, float)) else '-':>5s}"
            f" {'yes' if r['coverage'] else 'absent':>6s} {str(r['language_covered']) if r['language_covered'] is not None else 'absent':>5s}"
            f" {r['chunks_live']:5d} {r['chunks_deleted']:5d}"
            f" {str(r['window_s']['mean']):>8s} {str(r['window_s']['max']):>7s}"
            f" {r['delivered_covered_s']:8.1f} {str(r['duration_s'] or '-'):>8s}"
            f" {('%.1f' % (100 * r['delivered_coverage'])) if r['delivered_coverage'] is not None else '-':>6s}"
            f" {('%.1f' % (100 * r['deleted_coverage'])) if r['deleted_coverage'] is not None else '-':>6s}")
    return "\n".join(lines)


__all__ = ["run", "report_from_db", "transcript_from_db", "union_seconds",
           "snapshot_sqlite", "format_table", "text_stats"]
