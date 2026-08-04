"""Backend-free tests for text-similarity run collapsing (scrolling overlays).

The fixture is real material: 60 consecutive frames of TV Rain's news ticker,
OCR'd with tesseract `rus`, so the drift and the OCR noise in it are the ones
the code has to survive rather than a synthetic sliding window.
"""

import json
from itertools import pairwise
from pathlib import Path

from dirstral_annotator.recognizers.base import (
    MIN_MATCH_TOKENS,
    collapse_sightings,
    collapse_text_sightings,
    text_overlap,
    text_tokens,
)

FIXTURE = Path(__file__).parent / "fixtures" / "tvrain_ticker_reads.json"


def ticker_sightings(confidence: float = 0.7):
    data = json.loads(FIXTURE.read_text(encoding="utf-8"))
    return [(r["t"], r["text"], confidence) for r in data["reads"]]


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
