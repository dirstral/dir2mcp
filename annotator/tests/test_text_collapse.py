"""Backend-free tests for text-similarity run collapsing (scrolling overlays).

Both fixtures are real material: consecutive frames of TV Rain's news ticker
OCR'd with tesseract `rus`, so the drift and the OCR noise in them are the ones
the code has to survive rather than a synthetic sliding window. The 60-frame
run exercises collapsing; the 180-frame run is what the `text_overlap`
docstring's measurement table is derived from, and is re-derived here so the
two cannot drift apart.
"""

import json
from difflib import SequenceMatcher
from functools import lru_cache
from itertools import pairwise
from pathlib import Path
from statistics import median

import pytest

from dirstral_annotator.recognizers.base import (
    MIN_MATCH_TOKENS,
    _matched_tokens,
    collapse_sightings,
    collapse_text_sightings,
    text_overlap,
    text_tokens,
)

FIXTURE = Path(__file__).parent / "fixtures" / "tvrain_ticker_reads.json"
#: The longer run the `text_overlap` docstring's measurement table is derived
#: from. Separate from the collapsing fixture above because the table needs
#: pairs 30s apart to have an unrelated population at all, and 30s of ticker
#: does not contain them.
MEASURE_FIXTURE = Path(__file__).parent / "fixtures" / "tvrain_ticker_measure.json"


def ticker_sightings(confidence: float = 0.7):
    data = json.loads(FIXTURE.read_text(encoding="utf-8"))
    return [(r["t"], r["text"], confidence) for r in data["reads"]]


def measurement_reads():
    data = json.loads(MEASURE_FIXTURE.read_text(encoding="utf-8"))
    return [(r["t"], r["text"]) for r in data["reads"]]


# --- the measure -----------------------------------------------------------

def test_overlap_is_containment_not_symmetric_edit_distance():
    """A short correct read inside a long one is the same passage, not a 60%
    match: a half-occluded or clipped frame must not break a run."""
    long = "Европейские лидеры обсуждают создание буферной зоны на линии фронта"
    short = "обсуждают создание буферной зоны"
    assert text_overlap(long, short) == 1.0
    assert text_overlap(short, long) == 1.0


def test_overlap_survives_noise_inside_the_window():
    """The failure mode that rules out longest-common-SUBSTRING. Real OCR
    corrupts tokens in the middle of the band (`ЕКАМ` for `ЕВАМ`, `Навойне`
    for `На войне`), which breaks a contiguous run but not an ordered
    subsequence."""
    clean = "продажу Украине ракет увеличенной дальности ЕВАМ Пентагон На войне погибли"
    noisy = "продажу Украине ракет увеличенной дальности ЕКАМ Пентагон Навойне погибли"
    assert text_overlap(clean, noisy) >= 0.75


def test_overlap_is_a_true_lcs_not_difflibs_matching_blocks():
    """`SequenceMatcher.get_matching_blocks()` takes the longest block first and
    recurses either side, which is greedy: for `b c a x b c` against `a b c` it
    reports 2 of 3, though every token of the shorter read appears in order in
    the longer. A scroller can reissue a passage at the left edge while the
    same words are still leaving at the right, so the shape is reachable, and
    this function documents itself as containment. LCS answers 3 of 3."""
    longer = "буферной зоны на линии фронта обсуждают создание буферной зоны"
    shorter = "создание буферной зоны"
    assert text_overlap(longer, shorter) == 1.0


def test_symbol_only_reads_carry_no_tokens():
    """Unicode classes `|` as a maths symbol (`Sm`), not punctuation, so edge
    trimming alone leaves it standing. Tesseract emits a bare `|` for a column
    rule often enough that a frame whose only "text" is the divider between two
    poll bars would otherwise produce a cue."""
    assert text_tokens("|") == ()
    assert text_tokens("| + |") == ()
    assert text_overlap("|", "|") == 0.0
    assert text_tokens("53% | 10%") == ("53", "10")  # trailing % trims as before


def test_overlap_of_unrelated_russian_text_stays_low():
    a = "Европейские лидеры обсуждают создание буферной зоны на линии фронта"
    b = "Умер советский и российский композитор Родион Щедрин"
    assert text_overlap(a, b) < 0.2


def test_overlap_is_case_insensitive_and_punctuation_insensitive():
    assert text_overlap("«Медуза» и «Медиазона»", "медуза и медиазона") == 1.0


def test_empty_and_blank_reads_score_zero():
    assert text_overlap("", "что-нибудь") == 0.0
    assert text_overlap("   ", "") == 0.0


# --- tokenisation, Cyrillic ------------------------------------------------

def test_tokens_do_not_fold_short_i_onto_i():
    """`scorebug._fold` (NFKD plus combining-mark strip) is right for Nuñez and
    wrong here: it would turn й into и and ё into е, merging distinct Russian
    words. NFKC composes and keeps every letter."""
    assert text_tokens("мой") != text_tokens("мои")
    assert text_tokens("ёлка") != text_tokens("елка")
    assert text_overlap("край мой", "краи мои") == 0.0


def test_tokens_strip_edge_punctuation_by_unicode_category():
    """Russian quotation marks and the em dash the ticker uses as a separator
    come off exactly as an ASCII comma would; inner punctuation stays."""
    assert text_tokens("«Медуза», — и «Медиазона»") == ("Медуза", "и", "Медиазона")
    assert text_tokens("создание 40-километровой зоны") == (
        "создание", "40-километровой", "зоны")


# --- collapsing ------------------------------------------------------------

def test_static_overlay_collapses_exactly_as_identity_collapsing_does():
    """The baseball guarantee. Identical strings score 1.0 against the run
    anchor forever, so a fixed banner produces one cue with the same span,
    confidence and count `collapse_sightings` gives it."""
    sightings = [(t, "КИМ ЧЕН ЫН ВСТРЕТИТСЯ С ПУТИНЫМ", 0.8) for t in (10.0, 12.0, 14.0)]
    sightings.append((16.0, "КИМ ЧЕН ЫН ВСТРЕТИТСЯ С ПУТИНЫМ", 0.9))

    identity = collapse_sightings(sightings, source="overlay", event="banner",
                                  frame_gap=2.0)
    text = collapse_text_sightings(sightings, source="overlay", event="banner",
                                   frame_gap=2.0)
    assert len(identity) == len(text) == 1
    assert (text[0].start_s, text[0].end_s) == (identity[0].start_s, identity[0].end_s)
    assert text[0].confidence == identity[0].confidence == 0.9
    assert text[0].text == "КИМ ЧЕН ЫН ВСТРЕТИТСЯ С ПУТИНЫМ"


def test_scrolling_window_becomes_one_cue_where_identity_gives_one_per_frame():
    """A sliding window over one sentence: identity collapsing sees six
    different strings, similarity collapsing sees one passage."""
    words = ["Власти", "США", "одобрили", "продажу", "Украине", "ракет",
             "увеличенной", "дальности", "ERAM", "Пентагон"]
    sightings = [(float(i), " ".join(words[i:i + 8]), 0.6) for i in range(5)]

    assert len(collapse_sightings(sightings, source="t", event="e", frame_gap=1.0)) == 5
    cues = collapse_text_sightings(sightings, source="t", event="e", frame_gap=1.0)
    assert len(cues) == 1
    assert cues[0].start_s == 0.0 and cues[0].end_s == 5.0


def test_a_new_passage_opens_a_new_cue():
    sightings = [
        (0.0, "Европейские лидеры обсуждают создание буферной зоны", 0.6),
        (1.0, "лидеры обсуждают создание буферной зоны на линии", 0.6),
        (2.0, "Умер советский и российский композитор Родион Щедрин", 0.6),
        (3.0, "советский и российский композитор Родион Щедрин сегодня", 0.6),
    ]
    cues = collapse_text_sightings(sightings, source="t", event="e", frame_gap=1.0)
    assert len(cues) == 2
    assert cues[0].start_s == 0.0 and cues[1].start_s == 2.0


def test_a_real_time_gap_splits_a_run_even_when_the_text_matches():
    """The gap rule from `collapse_sightings` still applies: the same banner
    seen again ten minutes later is a second sighting, not one long one."""
    line = "КИМ ЧЕН ЫН ВСТРЕТИТСЯ С ПУТИНЫМ"
    sightings = [(0.0, line, 0.6), (2.0, line, 0.6), (600.0, line, 0.6)]
    cues = collapse_text_sightings(sightings, source="t", event="e", frame_gap=2.0)
    assert len(cues) == 2
    assert cues[1].start_s == 600.0


def test_short_reads_fall_back_to_exact_equality():
    """A clock badge is two tokens. At 0.5 similarity `10:24 МСК` and
    `10:25 МСК` share half their tokens and would collapse, erasing the minute
    that makes the badge worth reading."""
    assert MIN_MATCH_TOKENS > 2
    sightings = [(0.0, "10:24 МСК", 0.9), (1.0, "10:24 МСК", 0.9),
                 (2.0, "10:25 МСК", 0.9), (3.0, "10:25 МСК", 0.9)]
    cues = collapse_text_sightings(sightings, source="clock", event="clock",
                                   frame_gap=1.0)
    assert [c.text for c in cues] == ["10:24 МСК", "10:25 МСК"]


def test_blank_reads_are_dropped_not_collapsed():
    sightings = [(0.0, "", 0.5), (1.0, "   \n ", 0.5), (2.0, "!!! ---", 0.5)]
    assert collapse_text_sightings(sightings, source="t", event="e", frame_gap=1.0) == []


def test_cue_carries_the_longest_read_of_its_run():
    sightings = [(0.0, "лидеры обсуждают создание", 0.4),
                 (1.0, "лидеры обсуждают создание буферной зоны", 0.9),
                 (2.0, "обсуждают создание буферной", 0.5)]
    cues = collapse_text_sightings(sightings, source="t", event="e", frame_gap=1.0)
    assert len(cues) == 1
    assert cues[0].text == "лидеры обсуждают создание буферной зоны"
    assert cues[0].confidence == 0.9  # run max, as identity collapsing does


def test_text_cues_carry_no_entity_ids():
    """Ticker text is a topic, not an entity; nothing here resolves a roster."""
    cues = collapse_text_sightings([(0.0, "Умер композитор Родион Щедрин", 0.5)],
                                   source="t", event="e", frame_gap=1.0)
    assert cues[0].entity_ids == ()


def test_threshold_is_a_parameter_and_moves_the_boundary():
    sightings = ticker_sightings()
    loose = collapse_text_sightings(sightings, source="t", event="e", frame_gap=0.5,
                                    similarity=0.3)
    tight = collapse_text_sightings(sightings, source="t", event="e", frame_gap=0.5,
                                    similarity=0.8)
    assert len(loose) < len(tight)


# --- real footage ----------------------------------------------------------

def test_real_ticker_collapses_by_more_than_an_order_of_magnitude():
    """60 real frames of a scrolling ticker. Identity collapsing emits one cue
    per frame because every frame's OCR differs; this emits a handful."""
    sightings = ticker_sightings()
    identity = collapse_sightings(sightings, source="ticker", event="overlay_text",
                                  frame_gap=0.5)
    cues = collapse_text_sightings(sightings, source="ticker", event="overlay_text",
                                   frame_gap=0.5)
    assert len(identity) == len(sightings)  # 60 near-duplicate cues
    assert len(cues) <= 6
    assert len(identity) / len(cues) >= 10


def test_real_ticker_cues_tile_the_run_without_gaps_or_overlap_beyond_a_frame():
    cues = collapse_text_sightings(ticker_sightings(), source="ticker",
                                   event="overlay_text", frame_gap=0.5)
    assert cues[0].start_s == 0.0
    for earlier, later in pairwise(cues):
        assert later.start_s >= earlier.start_s
        # each cue ends one frame interval past its last read, i.e. where the
        # next begins: the ticker never stopped, so the timeline is covered.
        assert abs(earlier.end_s - later.start_s) <= 0.5


def test_real_ticker_cue_text_still_carries_a_readable_headline():
    """Collapsing must not cost the citation. One headline in the fixture is
    short enough to fit the ticker window whole; some cue has to hold it."""
    headline = "На войне погибли около 220 тысяч российских солдат"
    cues = collapse_text_sightings(ticker_sightings(), source="ticker",
                                   event="overlay_text", frame_gap=0.5)
    assert max(text_overlap(c.text, headline) for c in cues) >= 0.9


# --- the docstring's measurement table -------------------------------------

def _longest_common_substring(a, b):
    best, previous = 0, [0] * (len(b) + 1)
    for left in a:
        current = [0] * (len(b) + 1)
        for index, right in enumerate(b, start=1):
            if left == right:
                current[index] = previous[index - 1] + 1
                best = max(best, current[index])
        previous = current
    return best


@lru_cache(maxsize=1)
def _measured_populations():
    """Every measure in the docstring's table, over the committed reads.

    Cached: the three tests below share one pass, and the character-level row
    alone is a `SequenceMatcher` over 8664 pairs.

    Returns (measures, adjacent), where `measures` maps a row name to its
    same-passage and unrelated score populations and `adjacent` maps it to the
    scores at a fixed one-frame separation. Callers must not mutate either.
    """
    reads = measurement_reads()
    times = [t for t, _ in reads]
    texts = [text.casefold() for _, text in reads]
    tokens = [tuple(w.casefold() for w in text_tokens(text)) for _, text in reads]

    def substring(i, j):
        shorter = min(len(texts[i]), len(texts[j]))
        return _longest_common_substring(texts[i], texts[j]) / shorter if shorter else 0.0

    def char_ratio(i, j):
        return SequenceMatcher(None, texts[i], texts[j], autojunk=False).ratio()

    def token_lcs(i, j):
        shorter = min(len(tokens[i]), len(tokens[j]))
        return _matched_tokens(tokens[i], tokens[j]) / shorter if shorter else 0.0

    rows = {"substring": substring, "chars": char_ratio, "tokens": token_lcs}
    measures = {name: {"same": [], "unrelated": []} for name in rows}
    for i in range(len(times)):
        for j in range(i + 1, len(times)):
            gap = times[j] - times[i]
            if 0.5 <= gap <= 4.0:
                bucket = "same"
            elif gap >= 30.0:
                bucket = "unrelated"
            else:
                continue
            for name, fn in rows.items():
                measures[name][bucket].append(fn(i, j))

    adjacent = {name: [fn(i, i + 1) for i in range(len(reads) - 1)]
                for name, fn in rows.items()}
    return measures, adjacent


def test_every_row_of_the_docstring_table_still_reproduces():
    """`text_overlap`'s docstring justifies the measure with numbers. This
    re-derives ALL of them from the committed fixture so they cannot quietly go
    stale.

    They already did once: the table shipped a same-passage worst of 0.538 for
    the token measure, which is above what it actually scores (0.455) and above
    what the difflib-blocks measure that produced it could reach, since LCS is
    an upper bound on that. Nothing caught it because nothing recomputed it.
    """
    measures, _ = _measured_populations()

    assert len(measures["tokens"]["same"]) == 1404
    assert len(measures["tokens"]["unrelated"]) == 7260

    # row -> (same median, same worst, unrelated median, unrelated worst)
    published = {
        "substring": (0.882, 0.378, 0.053, 0.132),
        "chars": (0.902, 0.797, 0.248, 0.400),
        "tokens": (0.778, 0.455, 0.000, 0.231),
    }
    for row, (same_med, same_worst, unrel_med, unrel_worst) in published.items():
        same, unrelated = measures[row]["same"], measures[row]["unrelated"]
        assert median(same) == pytest.approx(same_med, abs=0.001), f"{row} same median"
        assert min(same) == pytest.approx(same_worst, abs=0.001), f"{row} same worst"
        assert median(unrelated) == pytest.approx(unrel_med, abs=0.001), f"{row} unrelated median"
        assert max(unrelated) == pytest.approx(unrel_worst, abs=0.001), f"{row} unrelated worst"


def test_the_dynamic_range_argument_still_holds():
    """The prose around the table, which is what actually picks the measure:
    the coincidence floors, the usable bands they leave, and their ratios."""
    measures, _ = _measured_populations()

    # Characters: floor 0.248 median / 0.400 worst, band about 0.40 to 0.90.
    chars = measures["chars"]
    assert median(chars["unrelated"]) == pytest.approx(0.248, abs=0.001)
    assert max(chars["unrelated"]) == pytest.approx(0.400, abs=0.001)
    assert median(chars["same"]) / max(chars["unrelated"]) == pytest.approx(2.3, abs=0.05)

    # Tokens: floor 0.000 median / 0.231 worst, band 0.23 to 0.78, factor 3.4.
    tokens = measures["tokens"]
    assert median(tokens["unrelated"]) == pytest.approx(0.000, abs=0.001)
    assert max(tokens["unrelated"]) == pytest.approx(0.231, abs=0.001)
    assert median(tokens["same"]) / max(tokens["unrelated"]) == pytest.approx(3.4, abs=0.05)


def test_the_anchor_decay_figures_still_reproduce():
    """The decay the docstring quotes against a fixed anchor, which is the
    claim that the threshold is not balanced on a knife edge."""
    reads = measurement_reads()
    times = [t for t, _ in reads]
    tokens = [tuple(w.casefold() for w in text_tokens(text)) for _, text in reads]

    def score(i, j):
        shorter = min(len(tokens[i]), len(tokens[j]))
        return _matched_tokens(tokens[i], tokens[j]) / shorter if shorter else 0.0

    for lag, want in ((1.0, 0.818), (10.0, 0.500), (15.0, 0.333), (30.0, 0.000)):
        at_lag = [score(i, j)
                  for i in range(len(times))
                  for j in range(i + 1, len(times))
                  if abs(times[j] - times[i] - lag) < 1e-9]
        assert median(at_lag) == pytest.approx(want, abs=0.001), f"lag {lag}s"


def test_this_measure_is_steadier_than_a_substring_at_a_fixed_separation():
    """The docstring rules longest-common-SUBSTRING out by its spread rather
    than its floor, and quotes both the endpoints and the ratios: adjacent
    frames span 0.378 to 1.000 under a substring (2.6x) against 0.636 to 1.000
    under this measure (1.6x)."""
    _, adjacent = _measured_populations()

    substring, ours = adjacent["substring"], adjacent["tokens"]
    assert min(substring) == pytest.approx(0.378, abs=0.001)
    assert max(substring) == pytest.approx(1.000, abs=0.001)
    assert min(ours) == pytest.approx(0.636, abs=0.001)
    assert max(ours) == pytest.approx(1.000, abs=0.001)
    assert max(substring) / min(substring) == pytest.approx(2.6, abs=0.05)
    assert max(ours) / min(ours) == pytest.approx(1.6, abs=0.05)


def test_a_cue_may_not_end_before_its_last_observation():
    """`cue_gap` is how far a cue runs past its last read. Negative is not a
    shorter cue, it is a cue that ends before the overlay was last seen: with
    one sighting `Cue` refuses it outright, and with several it silently claims
    a span the text had already left."""
    sightings = [(0.0, "Европейские лидеры обсуждают создание буферной зоны", 0.6)]
    with pytest.raises(ValueError):
        collapse_text_sightings(sightings, source="t", event="e", frame_gap=1.0,
                                cue_gap=-1.0)


def test_the_join_tolerance_and_the_trailing_extension_are_separable():
    """They are the same number only when reads arrive one sampling interval
    apart. A reader that samples sparsely while it searches needs to join
    across the wider gap while its cues still end one frame later, or
    consecutive passages carry overlapping spans."""
    line = "Европейские лидеры обсуждают создание буферной зоны на линии фронта"
    sightings = [(0.0, line, 0.6), (8.0, line, 0.6)]

    (joined,) = collapse_text_sightings(sightings, source="t", event="e",
                                        frame_gap=8.0, cue_gap=2.0)
    assert joined.start_s == 0.0
    assert joined.end_s == 10.0  # last read plus the FRAME, not the stride

    # Without the wider join tolerance the same reads are two sightings.
    assert len(collapse_text_sightings(sightings, source="t", event="e",
                                       frame_gap=2.0)) == 2
