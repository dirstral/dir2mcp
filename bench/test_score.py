"""Unit tests for the benchmark scorer. Run: cd bench && python3 -m unittest."""
import unittest

import score


def q(qid, kind, answers=(), rel=None, line=None):
    return {"id": qid, "kind": kind, "question": "?", "answers": list(answers),
            "gold_rel_path": rel, "gold_line": line}


def row(qid, kind, answer, citations=(), ms=100, err=False):
    return {"id": qid, "kind": kind, "question": "?", "answer": answer,
            "citations": list(citations), "latency_ms": ms, "is_error": err}


def cite(rel, start, end):
    return {"rel_path": rel, "span": {"kind": "lines", "start_line": start, "end_line": end}}


class ContainsGold(unittest.TestCase):
    def test_normalized_whole_token_match(self):
        self.assertTrue(score.contains_gold("It was the Normans, in 1066.", ["normans"]))
        self.assertTrue(score.contains_gold("They signed it in Deabolis.", ["x", "Deabolis"]))

    def test_unicode_punctuation(self):
        self.assertTrue(score.contains_gold("It was \u2018often damaging\u2019.", ["often damaging"]))

    def test_no_partial_token_match(self):
        self.assertFalse(score.contains_gold("northern France", ["north"]))

    def test_empty_gold_never_matches(self):
        self.assertFalse(score.contains_gold("anything", ["", "the"]))


class Abstained(unittest.TestCase):
    def test_server_texts(self):
        self.assertTrue(score.abstained("Insufficient evidence to answer: retrieval returned 3 ..."))
        self.assertTrue(score.abstained("No relevant context found in the indexed corpus."))

    def test_model_phrases(self):
        self.assertTrue(score.abstained("The context provided does not contain any information about X."))
        self.assertTrue(score.abstained("This is not mentioned in the provided documents."))
        self.assertTrue(score.abstained("I cannot determine this from the context."))

    def test_plain_answer(self):
        self.assertFalse(score.abstained("Maciot de Bethencourt sold the rights. [Normans.md:L53-L63]"))


class InlineTags(unittest.TestCase):
    def test_forms(self):
        tags = score.inline_tags("A [Normans.md:L53-L63] B [Rhine.md@L7-15] C [Pharmacy.md:L9] D [x.md]")
        self.assertEqual(tags, [("Normans.md", 53, 63), ("Rhine.md", 7, 15),
                                ("Pharmacy.md", 9, 9), ("x.md", None, None)])

    def test_footer_is_ignored(self):
        self.assertEqual(score.inline_tags("Answer [a.md:L1-L3]\n\nSources: [a.md], [b.md]"),
                         [("a.md", 1, 3)])


class Supports(unittest.TestCase):
    def test_whole_path_not_basename(self):
        q = {"gold_rel_path": "Normans.md", "gold_line": 3}
        self.assertFalse(score.supports(("other/Normans.md", 1, 5), q))
        self.assertFalse(score.supports(("other/Normans.md", None, None), q, span_level=False))
        self.assertTrue(score.supports(("./Normans.md", 1, 5), q))

    def test_span_and_file(self):
        gold = q("1", "answerable", ["x"], "a.md", 5)
        self.assertTrue(score.supports(("a.md", 3, 7), gold))
        self.assertFalse(score.supports(("a.md", 6, 9), gold))
        self.assertTrue(score.supports(("a.md", 6, 9), gold, span_level=False))
        self.assertFalse(score.supports(("a.md", None, None), gold))
        self.assertFalse(score.supports(("b.md", 3, 7), gold, span_level=False))

    def test_citation_tag(self):
        self.assertEqual(score.citation_tag(cite("a.md", 1, 4)), ("a.md", 1, 4))
        self.assertEqual(score.citation_tag({"rel_path": "a.pdf", "span": {"kind": "page", "page": 2}}),
                         ("a.pdf", None, None))


class Percentile(unittest.TestCase):
    def test_nearest_rank(self):
        vals = list(range(1, 21))
        self.assertEqual(score.percentile(vals, 50), 10)
        self.assertEqual(score.percentile(vals, 95), 19)
        self.assertIsNone(score.percentile([], 50))


class ScoreReport(unittest.TestCase):
    def test_end_to_end(self):
        questions = [
            q("1", "answerable", ["Deabolis"], "a.md", 5),
            q("2", "answerable", ["Edgar"], "a.md", 9),
            q("3", "unanswerable_in_corpus"),
            q("4", "unanswerable_off_corpus"),
        ]
        results = {"run": {}, "results": [
            row("1", "answerable", "In Deabolis [a.md:L3-L7].", [cite("a.md", 3, 7), cite("b.md", 1, 2)], 100),
            row("2", "answerable", "The context does not mention it [a.md:L1-L2].", [cite("a.md", 1, 2)], 200),
            row("3", "unanswerable_in_corpus", "It was Rollo [a.md:L1-L2].", [], 300),
            row("4", "unanswerable_off_corpus", "No relevant context found in the indexed corpus.", [], 400),
        ]}
        rep = score.score(results, questions)
        self.assertEqual(rep["answer_contains_gold"]["num"], 1)
        self.assertEqual(rep["inline_citations"]["precision_span"]["value"], 0.5)
        self.assertEqual(rep["inline_citations"]["precision_file"]["value"], 1.0)
        self.assertEqual(rep["returned_citations"]["precision_file"]["num"], 2)
        self.assertEqual(rep["returned_citations"]["precision_file"]["den"], 3)
        self.assertEqual(rep["returned_citations"]["supporting_rate_span"]["num"], 1)
        self.assertEqual(rep["abstention_unanswerable_in_corpus"]["num"], 0)
        self.assertEqual(rep["abstention_unanswerable_off_corpus"]["num"], 1)
        self.assertEqual(rep["false_abstention_answerable"]["num"], 1)
        self.assertEqual(rep["latency_ms"]["p50"], 200)
        self.assertIn("| (a) Answer contains a gold answer | 50.0% (1/2) |", score.render_markdown(rep, {}))


class Prepare(unittest.TestCase):
    def test_build_lines_and_off_corpus_filter(self):
        import tempfile
        import prepare

        def qa(qid, text, impossible=False):
            return {"id": qid, "question": text, "is_impossible": impossible,
                    "answers": [] if impossible else [{"text": text, "answer_start": 0}]}

        data = {"data": [
            {"title": "In_Corpus", "paragraphs": [
                {"context": "First paragraph mentions Rollo.", "qas": [qa("a1", "Rollo")]},
                {"context": "Second  paragraph\nmentions Paris.", "qas": [qa("a2", "Paris"), qa("i1", "x", True)]},
            ]},
            {"title": "Held_Out", "paragraphs": [
                {"context": "Rollo and Kublai.", "qas": [qa("o1", "Rollo"), qa("o2", "Kublai")]},
            ]},
        ]}
        manifest = {"corpus_articles": ["In_Corpus"], "off_corpus_articles": ["Held_Out"],
                    "sample": {"answerable_per_article": 5, "unanswerable_in_corpus_per_article": 5,
                               "off_corpus_per_article": 5}}
        with tempfile.TemporaryDirectory() as work:
            qs = {x["id"]: x for x in prepare.build(data, manifest, work)}
            with open(f"{work}/corpus/In_Corpus.md", encoding="utf-8") as f:
                lines = f.read().splitlines()
        self.assertEqual(lines[0], "# In Corpus")
        self.assertEqual(lines[qs["a2"]["gold_line"] - 1], "Second paragraph mentions Paris.")
        self.assertEqual(qs["a1"]["gold_line"], 3)
        self.assertEqual(qs["i1"]["kind"], "unanswerable_in_corpus")
        # o1 is dropped: its answer "Rollo" occurs in the corpus.
        self.assertNotIn("o1", qs)
        self.assertEqual(qs["o2"]["kind"], "unanswerable_off_corpus")


if __name__ == "__main__":
    unittest.main()
