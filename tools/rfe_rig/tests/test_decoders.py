import os
import tempfile
import unittest

from tools.rfe_rig import decoders, report, transcripts
from tools.rfe_rig.tests.test_coverage import make_fixture


class ParseSpecTest(unittest.TestCase):
    def test_http(self):
        kind, base, params = decoders.parse_spec(
            "http://127.0.0.1:9010/?model=large-v3-multi&name=whisper-9010")
        self.assertEqual(kind, "http")
        self.assertEqual(base, "http://127.0.0.1:9010")
        self.assertEqual(params, {"model": "large-v3-multi", "name": "whisper-9010"})

    def test_sqlite_mms_fw(self):
        self.assertEqual(decoders.parse_spec("sqlite:/x/.dir2mcp"), ("sqlite", "/x/.dir2mcp", {}))
        self.assertEqual(decoders.parse_spec("mms:kir"), ("mms", "kir", {}))
        self.assertEqual(decoders.parse_spec("fw:/m/small-kyrgyz-ct2?language=ky"),
                         ("fw", "/m/small-kyrgyz-ct2", {"language": "ky"}))

    def test_unknown(self):
        with self.assertRaises(decoders.DecoderError):
            decoders.parse_spec("magic:thing")


class SegmentsFromOpenAITest(unittest.TestCase):
    def test_words_attached_from_top_level(self):
        resp = {"segments": [{"id": 0, "start": 0.0, "end": 2.0, "text": " hi there"},
                             {"id": 1, "start": 2.0, "end": 4.0, "text": " again"}],
                "words": [{"word": "hi", "start": 0.1, "end": 0.5},
                          {"word": "there", "start": 0.6, "end": 1.0},
                          {"word": "again", "start": 2.5, "end": 3.0}]}
        segs = decoders._segments_from_openai(resp)
        self.assertEqual(len(segs), 2)
        self.assertEqual([w["word"] for w in segs[0]["words"]], ["hi", "there"])
        self.assertEqual([w["word"] for w in segs[1]["words"]], ["again"])

    def test_no_words(self):
        segs = decoders._segments_from_openai({"segments": [{"start": 0, "end": 1, "text": "x"}]})
        self.assertNotIn("words", segs[0])

    def test_multipart_shape(self):
        ctype, body = decoders._multipart([("model", "m")], "file", "a.wav", b"RIFF")
        self.assertTrue(ctype.startswith("multipart/form-data; boundary="))
        self.assertIn(b'name="model"\r\n\r\nm\r\n', body)
        self.assertIn(b'filename="a.wav"', body)
        self.assertTrue(body.endswith(b"--\r\n"))


class SqliteDecoderAndCacheTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = os.path.join(self.tmp.name, ".dir2mcp")
        os.makedirs(self.state)
        make_fixture(os.path.join(self.state, "meta.sqlite"))
        self.results = os.path.join(self.tmp.name, "results")
        # the "recording" only needs a basename; ffprobe on a missing file yields None
        self.recording = os.path.join(self.tmp.name, "a.flac")

    def tearDown(self):
        self.tmp.cleanup()

    def test_extract_then_cache_hit(self):
        logs = []
        t, path, cached = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                              log=logs.append)
        self.assertFalse(cached)
        self.assertTrue(os.path.exists(path))
        self.assertEqual(t["decoder"]["kind"], "sqlite")
        self.assertEqual(t["decoder"]["name"], "dir2mcp-whisper-large-v3-multi")
        self.assertEqual(t["decoder"]["version"], "large-v3-multi|hashA0000000")
        self.assertEqual(t["language"], "ky")
        self.assertEqual(len(t["segments"]), 2)
        self.assertEqual(t["segments"][1]["words"][0]["word"], "үч")
        self.assertIsNone(t["duration_s"])
        t2, path2, cached2 = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                                 log=logs.append)
        self.assertTrue(cached2)
        self.assertEqual(path2, path)
        self.assertEqual(t2["segments"], t["segments"])
        self.assertTrue(any("cache hit" in m for m in logs))

    def test_force_redecodes(self):
        decoders.transcribe("sqlite:" + self.state, self.recording, self.results, log=lambda m: None)
        _t, _p, cached = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                             log=lambda m: None, force=True)
        self.assertFalse(cached)

    def test_reindexed_state_is_a_cache_miss(self):
        import sqlite3
        _t, path1, _c = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                            log=lambda m: None)
        con = sqlite3.connect(os.path.join(self.state, "meta.sqlite"))
        con.execute("update representations set rep_hash='hashA-reindexed' where rep_id=10")
        con.commit()
        con.close()
        t2, path2, cached = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                                log=lambda m: None)
        self.assertFalse(cached)
        self.assertNotEqual(path1, path2)
        self.assertEqual(t2["decoder"]["version"], "large-v3-multi|hashA-reinde")
        self.assertTrue(os.path.exists(path1) and os.path.exists(path2))

    def test_decoder_reuse_across_recordings(self):
        dec = decoders.Decoder("sqlite:" + self.state, log=lambda m: None)
        _t, p_a, _c = decoders.transcribe("sqlite:x", self.recording, self.results,
                                          log=lambda m: None, decoder=dec)
        _t, p_b, _c = decoders.transcribe("sqlite:x", os.path.join(self.tmp.name, "b.flac"),
                                          self.results, log=lambda m: None, decoder=dec)
        self.assertNotEqual(p_a, p_b)
        self.assertIn("hashB", p_b)

    def test_missing_recording_in_state(self):
        with self.assertRaises(decoders.DecoderError):
            decoders.transcribe("sqlite:" + self.state, os.path.join(self.tmp.name, "nope.flac"),
                                self.results, log=lambda m: None)

    def test_cache_path_and_stats(self):
        p = transcripts.cache_path("/r", "/x/rec.flac", "dec__1")
        self.assertEqual(p, os.path.join("/r", "transcripts", "rec", "dec__1.json"))
        t = transcripts.build("/x/rec.flac", 10.0, {"kind": "test", "name": "n", "version": "v"},
                              [{"start": 0, "end": 4, "text": "a b", "words": [
                                  {"start": 0, "end": 1, "word": "a"}, {"start": 1, "end": 2, "word": " "}]}])
        self.assertEqual(t["decoder"]["id"], "n__v")
        self.assertEqual(len(t["segments"][0]["words"]), 1)
        self.assertEqual(transcripts.text_stats(t),
                         {"segments": 1, "words": 1, "chars": 3, "segment_time_s": 4.0})

    def test_report_over_results(self):
        from tools.rfe_rig import agreement
        t, _p, _c = decoders.transcribe("sqlite:" + self.state, self.recording, self.results,
                                        log=lambda m: None)
        rep = agreement.compare(t, t)
        agr_dir = os.path.join(self.results, "agreement")
        os.makedirs(agr_dir)
        with open(os.path.join(agr_dir, agreement.output_name(self.recording, rep["a"], rep["b"])),
                  "w", encoding="utf-8") as fh:
            import json
            json.dump(rep, fh)
        summary_dir = os.path.join(self.tmp.name, "baseline")
        s, md = report.write(self.results, summary_dir=summary_dir)
        self.assertIsNone(s["ask"])
        self.assertEqual(len(s["transcripts"]), 1)
        self.assertEqual(len(s["agreements"]), 1)
        self.assertIn("ask.json missing", md)
        for d in (self.results, summary_dir):
            for name in ("summary.md", "summary.json", "agreement.csv"):
                self.assertTrue(os.path.exists(os.path.join(d, name)), (d, name))
        with open(os.path.join(summary_dir, "agreement.csv"), encoding="utf-8") as fh:
            lines = fh.read().splitlines()
        self.assertEqual(lines[0].split(",")[:3], ["recording", "a", "b"])
        self.assertEqual(len(lines), 2)
        self.assertIn("a.flac", lines[1])
        self.assertIn(",1.0,", lines[1])  # coverage_by_agreement of a self-comparison


if __name__ == "__main__":
    unittest.main()
