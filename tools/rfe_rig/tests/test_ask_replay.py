import os
import unittest

from tools.rfe_rig import ask

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.join(HERE, "..", "fixtures", "ask_2026-09-12_main-final.json")


class ReplayTest(unittest.TestCase):
    """The #964 number, reproduced from the committed answers.

    main-final-ask.json is the measure.py output for shipped main 414efce3
    on 2026-09-12 (12 questions, two runs each). detect.py scored it 24 of
    24; this guards that the ported detector still does.
    """

    def test_reproduces_24_of_24(self):
        rep = ask.replay(FIXTURE)
        s = rep["summary"]
        self.assertEqual(s["asked"], 24)
        self.assertEqual(s["in_language"], 24, s["failing"])
        self.assertEqual(s["undecidable"], 0)
        self.assertEqual(s["errors"], 0)
        self.assertEqual(s["fallback_answers"], 0)
        self.assertEqual(rep["replayed_from"], "ask_2026-09-12_main-final.json")
        self.assertEqual(max(r["run"] for r in rep["rows"]), 2)

    def test_stored_character_detector_was_weaker(self):
        # measure.py's own detector recorded 21 of 24; the word-voting one
        # from detect.py is what the rig ports.
        rep = ask.replay(FIXTURE)
        stored_ok = sum(1 for r in rep["rows"] if r["stored_got"] == r["want"])
        self.assertEqual(stored_ok, 21)

    def test_tag_count_matches_stored(self):
        rep = ask.replay(FIXTURE)
        self.assertGreater(rep["summary"]["tagged"], 0)


class FallbackTest(unittest.TestCase):
    def test_fallback_shape(self):
        self.assertTrue(ask.is_fallback("Question: x? Top context: - a.flac: y"))
        self.assertFalse(ask.is_fallback("Кулеба каже про НАТО [a.flac@t=1:00]."))
        self.assertFalse(ask.is_fallback(""))

    def test_answer_source_is_authoritative(self):
        # SPEC 9.4.5 (dir2mcp #1019): the daemon marks a retrieval-only answer
        # itself. The field wins over the text shape in both directions, so a
        # generated answer that happens to start with "Question:" is not a
        # fallback, and a retrieval-only answer in any shape is.
        self.assertFalse(ask.is_fallback("Question: x? Top context: y",
                                         {"answer_source": "generated"}))
        self.assertTrue(ask.is_fallback("Plain retrieved text.",
                                        {"answer_source": "retrieval_only",
                                         "answer_source_reason": "generator_error"}))
        row = ask.score("en", "Plain retrieved text about the budget.",
                        {"answer_source": "retrieval_only"})
        self.assertFalse(row["generated"])
        self.assertEqual(row["answer_source"], "retrieval_only")

    def test_score_row(self):
        row = ask.score("ru", "Question: О чём? Top context: - a.flac: Это ответ о том, что было.",
                        {"citations": [{"chunk_id": 1}, {"chunk_id": 2}]})
        self.assertEqual(row["got"], "ru")
        self.assertFalse(row["generated"])
        self.assertEqual(row["citations_n"], 2)
        self.assertEqual(row["tags"], 0)

    def test_summary_counts_fallbacks(self):
        rows = [ask.score("ru", "Question: О чём? Top context: - f.flac: Это ответ о том, что было сказано.",
                          {}) | {"q": "a", "run": 1},
                ask.score("en", "It is an answer [f.flac@t=0:10].", {"citations": [1]}) | {"q": "b", "run": 1}]
        s = ask.summarize(rows)
        self.assertEqual(s["fallback_answers"], 1)
        self.assertEqual(s["cited_structured"], 1)
        self.assertEqual(s["tagged"], 1)
        self.assertEqual(s["in_language"], 2)
        text = ask.format_summary({"summary": s})
        self.assertIn("FALLBACK  1/2", text)


if __name__ == "__main__":
    unittest.main()
