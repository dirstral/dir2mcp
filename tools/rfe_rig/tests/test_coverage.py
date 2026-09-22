import json
import os
import sqlite3
import tempfile
import unittest

from tools.rfe_rig import coverage

SCHEMA = [
    "CREATE TABLE documents (doc_id INTEGER PRIMARY KEY, rel_path TEXT NOT NULL UNIQUE,"
    " doc_type TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'ok', deleted INTEGER NOT NULL DEFAULT 0,"
    " skip_reason TEXT NOT NULL DEFAULT '')",
    "CREATE TABLE representations (rep_id INTEGER PRIMARY KEY, doc_id INTEGER NOT NULL,"
    " rep_type TEXT NOT NULL, rep_hash TEXT NOT NULL, meta_json TEXT NOT NULL DEFAULT '',"
    " created_unix INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0)",
    "CREATE TABLE chunks (chunk_id INTEGER PRIMARY KEY, rep_id INTEGER, ordinal INTEGER NOT NULL DEFAULT 0,"
    " rel_path TEXT NOT NULL, doc_type TEXT NOT NULL DEFAULT 'audio', rep_type TEXT NOT NULL DEFAULT 'transcript',"
    " text TEXT NOT NULL, language TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0)",
    "CREATE TABLE spans (span_id INTEGER PRIMARY KEY, chunk_id INTEGER NOT NULL, span_kind TEXT NOT NULL,"
    " start INTEGER NOT NULL, end INTEGER NOT NULL, extra_json TEXT)",
]


def make_fixture(path):
    con = sqlite3.connect(path)
    for s in SCHEMA:
        con.execute(s)
    con.execute("insert into documents values (1, 'a.flac', 'audio', 'ok', 0, '')")
    con.execute("insert into documents values (2, 'b.flac', 'audio', 'ok', 0, '')")
    meta_a = json.dumps({"source": "stt", "language": "ky", "language_source": "detected",
                         "language_confidence": 0.9, "provider": "whisper", "model": "large-v3-multi",
                         "coverage": {"windows_attempted": 3, "windows_decoded": 2,
                                      "ranges": [{"start_ms": 0, "end_ms": 60000}],
                                      "decoded_ms": 60000, "duration_ms": 90000},
                         "language_covered": False})
    meta_b = json.dumps({"source": "stt", "provider": "whisper", "model": "large-v3-multi"})
    con.execute("insert into representations values (10, 1, 'transcript', 'hashA000000000', ?, 0, 0)", (meta_a,))
    con.execute("insert into representations values (11, 2, 'transcript', 'hashB', ?, 0, 0)", (meta_b,))
    # a.flac: two live chunks (overlapping spans) and one deleted chunk
    con.execute("insert into chunks values (1, 10, 0, 'a.flac', 'audio', 'transcript', 'бир эки', 'ky', 0)")
    con.execute("insert into chunks values (2, 10, 1, 'a.flac', 'audio', 'transcript', 'үч төрт беш', 'ky', 0)")
    con.execute("insert into chunks values (3, 10, 2, 'a.flac', 'audio', 'transcript', 'old', 'ky', 1)")
    con.execute("insert into spans values (1, 1, 'time', 0, 20000, ?)",
                (json.dumps({"words": [{"t": 0, "d": 500, "w": "бир"}, {"t": 10000, "d": 500, "w": "эки"}]}),))
    con.execute("insert into spans values (2, 2, 'time', 15000, 40000, ?)",
                (json.dumps({"words": [{"t": 15000, "d": 500, "w": "үч"}, {"t": 20000, "d": 500, "w": "төрт"},
                                       {"t": 39000, "d": 500, "w": "беш"}]}),))
    con.execute("insert into spans values (3, 3, 'time', 0, 7000, NULL)")
    # b.flac: one live chunk with no words
    con.execute("insert into chunks values (4, 11, 0, 'b.flac', 'audio', 'transcript', 'text only', '', 0)")
    con.execute("insert into spans values (4, 4, 'time', 1000, 4000, NULL)")
    con.commit()
    con.close()


class CoverageTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = os.path.join(self.tmp.name, ".dir2mcp")
        os.makedirs(self.state)
        self.db = os.path.join(self.state, "meta.sqlite")
        make_fixture(self.db)

    def tearDown(self):
        self.tmp.cleanup()

    def test_union_seconds(self):
        self.assertEqual(coverage.union_seconds([(0, 20000), (15000, 40000)]), 40.0)
        self.assertEqual(coverage.union_seconds([(0, 10000), (20000, 30000)]), 20.0)
        self.assertEqual(coverage.union_seconds([(5000, 5000)]), 0.0)
        self.assertEqual(coverage.union_seconds([]), 0.0)

    def test_report_rows(self):
        rep = coverage.run(self.state)
        self.assertEqual(rep["schema"], coverage.SCHEMA)
        rows = {r["rel_path"]: r for r in rep["recordings"]}
        a = rows["a.flac"]
        self.assertEqual(a["language"], "ky")
        self.assertEqual(a["language_source"], "detected")
        self.assertAlmostEqual(a["language_confidence"], 0.9)
        self.assertEqual(a["coverage"]["windows_decoded"], 2)
        self.assertIs(a["language_covered"], False)
        self.assertEqual(a["chunks_live"], 2)
        self.assertEqual(a["chunks_deleted"], 1)
        self.assertEqual(a["window_s"], {"mean": 22.5, "median": 22.5, "min": 20.0, "max": 25.0})
        self.assertEqual(a["words_live"], 5)
        self.assertEqual(a["delivered_covered_s"], 40.0)
        self.assertEqual(a["deleted_covered_s"], 7.0)
        self.assertIsNone(a["duration_s"])
        self.assertIsNone(a["delivered_coverage"])
        self.assertIsNone(a["deleted_coverage"])
        self.assertEqual(a["chunk_languages"], {"ky": 2})
        b = rows["b.flac"]
        self.assertIsNone(b["language"])
        self.assertIsNone(b["coverage"])
        self.assertIsNone(b["language_covered"])
        self.assertEqual(b["chunks_live"], 1)
        self.assertEqual(b["delivered_covered_s"], 3.0)

    def test_snapshot_does_not_open_original(self):
        with tempfile.TemporaryDirectory() as t2:
            copy = coverage.snapshot_sqlite(self.state, t2)
            self.assertNotEqual(os.path.realpath(copy), os.path.realpath(self.db))
            self.assertTrue(os.path.exists(copy))

    def test_transcript_from_db(self):
        meta, segs = coverage.transcript_from_db(self.db, "a.flac")
        self.assertEqual(meta["model"], "large-v3-multi")
        self.assertEqual(meta["rep_hash"], "hashA000000000")
        self.assertEqual(len(segs), 2)
        self.assertEqual(segs[0]["start"], 0.0)
        self.assertEqual(segs[0]["end"], 20.0)
        self.assertEqual(segs[0]["words"][1], {"start": 10.0, "end": 10.5, "word": "эки"})
        self.assertNotIn("words", segs and coverage.transcript_from_db(self.db, "b.flac")[1][0])
        self.assertEqual(coverage.transcript_from_db(self.db, "missing.flac"), (None, []))

    def test_format_table(self):
        text = coverage.format_table(coverage.run(self.state))
        self.assertIn("a.flac", text)
        self.assertIn("absent", text)


if __name__ == "__main__":
    unittest.main()
