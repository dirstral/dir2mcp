import unittest

from tools.rfe_rig import agreement
from tools.rfe_rig.windows import bin_tokens, window_bounds, window_count


def _t(name, segments, duration=90.0):
    return {"schema": "rfe_rig.transcript.v1", "recording": "x.wav", "duration_s": duration,
            "decoder": {"id": name, "kind": "test", "name": name, "version": "1"},
            "segments": segments}


class WindowTest(unittest.TestCase):
    def test_window_count(self):
        self.assertEqual(window_count(90.0, 30.0), 3)
        self.assertEqual(window_count(90.1, 30.0), 4)
        self.assertEqual(window_count(0.0, 30.0), 1)
        with self.assertRaises(ValueError):
            window_count(10.0, 0.0)

    def test_bounds(self):
        self.assertEqual(window_bounds(2, 30.0), (60.0, 90.0))

    def test_words_bin_by_word_start(self):
        seg = {"start": 25.0, "end": 35.0, "text": "one two three",
               "words": [{"start": 25.0, "end": 26.0, "word": "One,"},
                         {"start": 29.99, "end": 30.5, "word": "two"},
                         {"start": 30.0, "end": 31.0, "word": "three."}]}
        bins = bin_tokens([seg], 60.0, 30.0)
        self.assertEqual(bins, [["one", "two"], ["three"]])

    def test_untimed_segment_spreads_proportionally(self):
        # 40 s segment across 30 s boundary: tokens 0-2 fall before 30 s,
        # the rest after (equal-length tokens, 10 s each).
        seg = {"start": 10.0, "end": 50.0, "text": "aa bb cc dd"}
        bins = bin_tokens([seg], 60.0, 30.0)
        self.assertEqual(bins, [["aa", "bb"], ["cc", "dd"]])

    def test_tokens_past_duration_land_in_last_window(self):
        seg = {"start": 95.0, "end": 96.0, "text": "late"}
        bins = bin_tokens([seg], 90.0, 30.0)
        self.assertEqual(bins[-1], ["late"])
        self.assertEqual(len(bins), 3)

    def test_negative_start_clamped(self):
        seg = {"start": -1.0, "end": 1.0, "text": "early"}
        self.assertEqual(bin_tokens([seg], 30.0, 30.0), [["early"]])


class AgreementTest(unittest.TestCase):
    def test_summary_and_flags(self):
        a = _t("A", [{"start": 0, "end": 10, "text": "Крым это Украина"},
                     {"start": 30, "end": 40, "text": "один два три четыре"},
                     {"start": 60, "end": 70, "text": "только здесь"}])
        b = _t("B", [{"start": 0, "end": 10, "text": "крым это украина"},
                     {"start": 30, "end": 40, "text": "один два пять шесть"}])
        rep = agreement.compare(a, b, window_s=30.0, threshold=0.5)
        s = rep["summary"]
        self.assertEqual(s["windows_total"], 3)
        self.assertEqual(s["speech_windows"], 3)
        # window 0 identical, window 1 wer 0.5 (agrees at threshold), window 2 B empty
        self.assertEqual([w["agree"] for w in rep["windows"]], [True, True, False])
        self.assertEqual(s["agreeing_windows"], 2)
        self.assertAlmostEqual(s["agree_fraction_all"], 2 / 3, places=3)
        self.assertAlmostEqual(s["coverage_by_agreement"], 2 / 3, places=3)
        self.assertAlmostEqual(s["overall_wer"], (0 + 2 + 2) / (3 + 4 + 2), places=3)
        self.assertEqual(s["a_windows_with_text"], 3)
        self.assertEqual(s["b_windows_with_text"], 2)
        self.assertEqual(rep["worst"][0]["i"], 2)

    def test_silence_windows_are_not_speech(self):
        a = _t("A", [{"start": 0, "end": 5, "text": "hello"}], duration=120.0)
        b = _t("B", [{"start": 0, "end": 5, "text": "hello"}], duration=120.0)
        rep = agreement.compare(a, b)
        s = rep["summary"]
        self.assertEqual(s["windows_total"], 4)
        self.assertEqual(s["speech_windows"], 1)
        self.assertEqual(s["coverage_by_agreement"], 1.0)
        self.assertEqual(s["agree_fraction_all"], 0.25)

    def test_duration_falls_back_to_last_end(self):
        a = _t("A", [{"start": 0, "end": 65, "text": "x"}], duration=0)
        b = _t("B", [], duration=0)
        rep = agreement.compare(a, b)
        self.assertEqual(rep["summary"]["windows_total"], 3)

    def test_output_name(self):
        self.assertEqual(agreement.output_name("/x/rec.flac", "a__1", "b__2"),
                         "rec__a__1__vs__b__2.json")

    def test_format_summary_runs(self):
        a = _t("A", [{"start": 0, "end": 5, "text": "hello"}])
        rep = agreement.compare(a, a)
        text = agreement.format_summary(rep)
        self.assertIn("coverage by agreem. 1.000", text)


if __name__ == "__main__":
    unittest.main()
