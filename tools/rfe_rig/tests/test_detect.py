import unittest

from tools.rfe_rig.ask import summarize
from tools.rfe_rig.detect import citation_tags, detect


class DetectTest(unittest.TestCase):
    """Fixed strings shaped like the answers that produced the #964 numbers."""

    def test_ukrainian(self):
        lang, _ = detect("Патріарх Філарет говорить про те, що церква відокремлена від держави.")
        self.assertEqual(lang, "uk")

    def test_russian_without_distinctive_letters(self):
        # The measure.py character detector called this "ru?"; the word vote
        # makes it Russian, which is what lifted the score from 21 to 24.
        lang, _ = detect("Спрашивают, как он оценивает русский мир и говорит ли он, что он приходит туда.")
        self.assertEqual(lang, "ru")

    def test_kyrgyz_with_many_russian_letters(self):
        lang, _ = detect("Садыр Жапаров референдум тууралуу: Кыргызстанда 11-апрелде референдум өтөрүн, "
                         "анда жаңы баш мыйзамды кабыл алуу боюнча добуш берилерин айтат.")
        self.assertEqual(lang, "ky")

    def test_english(self):
        lang, ev = detect("The foreign minister says NATO membership remains the goal.")
        self.assertEqual(lang, "en")
        self.assertIn("latin", ev)

    def test_georgian(self):
        lang, _ = detect("ეს ინტერვიუ ეხება ვახტანგ კიკაბიძეს.")
        self.assertEqual(lang, "ka")

    def test_tie_is_unknown_not_guess(self):
        # one uk letter, one ru letter, no words: a tie
        lang, ev = detect("ї ы")
        self.assertEqual(lang, "unknown")
        self.assertIn("tie", ev)

    def test_no_evidence_is_unknown(self):
        lang, ev = detect("абв мнп")
        self.assertEqual(lang, "unknown")
        self.assertIn("no distinctive evidence", ev)

    def test_empty(self):
        self.assertEqual(detect("   ")[0], "empty")

    def test_citation_tags(self):
        text = ("Кулеба каже про НАТО [ukr_18126_kuleba_interview.flac@t=12:05] і додає"
                " [ukr_18126_kuleba_interview.flac@t=13:40-13:55].")
        self.assertEqual(citation_tags(text), 2)
        self.assertEqual(citation_tags("no tag [file] here"), 0)
        self.assertEqual(citation_tags(""), 0)


class AskSummaryTest(unittest.TestCase):
    def test_counts(self):
        rows = [
            {"want": "uk", "got": "uk", "tags": 1, "q": "a", "run": 1},
            {"want": "ru", "got": "unknown", "tags": 0, "q": "b", "run": 1},
            {"want": "en", "got": "error", "tags": 0, "q": "c", "run": 2},
        ]
        s = summarize(rows)
        self.assertEqual((s["asked"], s["in_language"], s["undecidable"], s["errors"], s["tagged"]),
                         (3, 1, 1, 1, 1))
        self.assertEqual([f["q"] for f in s["failing"]], ["b", "c"])


if __name__ == "__main__":
    unittest.main()
