import unittest

from tools.rfe_rig.textnorm import normalize, tokens
from tools.rfe_rig.wer import edit_distance, wer


class NormalizeTest(unittest.TestCase):
    def test_case_punctuation_and_spacing(self):
        self.assertEqual(normalize("  Крым: это Украина!  "), "крым это украина")

    def test_yo_folds_to_ye(self):
        self.assertEqual(normalize("Ещё"), "еще")

    def test_apostrophe_inside_word_keeps_one_token(self):
        self.assertEqual(tokens("п'ять м’яких"), ["пять", "мяких"])

    def test_digits_kept(self):
        self.assertEqual(tokens("в 2005 году"), ["в", "2005", "году"])

    def test_empty(self):
        self.assertEqual(normalize(""), "")
        self.assertEqual(tokens(None), [])

    def test_nfkc(self):
        # fullwidth digit folds to ASCII
        self.assertEqual(tokens("２０"), ["20"])


class WerTest(unittest.TestCase):
    def test_identical(self):
        self.assertEqual(edit_distance(["a", "b"], ["a", "b"]), 0)
        self.assertEqual(wer(["a", "b"], ["a", "b"]), (0, 0.0, 0.0))

    def test_substitution_insertion_deletion(self):
        self.assertEqual(edit_distance(["a", "b", "c"], ["a", "x", "c"]), 1)
        self.assertEqual(edit_distance(["a", "b", "c"], ["a", "b", "c", "d"]), 1)
        self.assertEqual(edit_distance(["a", "b", "c"], ["a", "c"]), 1)

    def test_wer_uses_reference_length(self):
        edits, w, ned = wer(["a", "b", "c", "d"], ["a", "b"])
        self.assertEqual(edits, 2)
        self.assertAlmostEqual(w, 0.5)
        self.assertAlmostEqual(ned, 0.5)

    def test_wer_can_exceed_one_ned_cannot(self):
        edits, w, ned = wer(["a"], ["x", "y", "z"])
        self.assertEqual(edits, 3)
        self.assertAlmostEqual(w, 3.0)
        self.assertAlmostEqual(ned, 1.0)

    def test_empty_reference(self):
        self.assertEqual(wer([], ["a", "b"]), (2, 1.0, 1.0))
        self.assertEqual(wer(["a", "b"], []), (2, 1.0, 1.0))
        self.assertEqual(wer([], []), (0, 0.0, 0.0))

    def test_symmetric_distance(self):
        a, b = "the quick brown fox".split(), "quick brown fox jumps".split()
        self.assertEqual(edit_distance(a, b), edit_distance(b, a))


if __name__ == "__main__":
    unittest.main()
