from dirstral_annotator.fusion import fuse
from dirstral_annotator.model import Cue

WEBB = ("player:webb-logan",)


def cue(source, start, end, conf, event="pitch", entities=WEBB, text=""):
    return Cue(source=source, start_s=start, end_s=end, event=event,
               entity_ids=entities, confidence=conf, text=text)


def test_agreeing_sources_merge_and_boost():
    anns = fuse([
        cue("playbyplay", 100.0, 108.0, 0.98, text="Pitch: Logan Webb to X"),
        cue("scorebug", 99.0, 110.0, 0.9),
        cue("face", 101.0, 106.0, 0.6),
    ])
    assert len(anns) == 1
    a = anns[0]
    assert a.start_s == 99.0 and a.end_s == 110.0
    assert a.sources == ("face", "playbyplay", "scorebug")
    # noisy-OR: 1 - 0.02*0.1*0.4
    assert abs(a.confidence - (1 - 0.02 * 0.1 * 0.4)) < 1e-6
    assert a.text == "Pitch: Logan Webb to X"


def test_repeat_cues_from_one_source_do_not_inflate():
    anns = fuse([cue("face", 10, 12, 0.5), cue("face", 11, 13, 0.5)])
    assert len(anns) == 1
    assert anns[0].confidence == 0.5  # max, not noisy-OR with itself


def test_different_entities_do_not_merge():
    anns = fuse([
        cue("scorebug", 10, 20, 0.9),
        cue("scorebug", 12, 18, 0.9, entities=("player:doval-camilo",)),
    ])
    assert len(anns) == 2


def test_different_events_do_not_merge():
    anns = fuse([cue("scorebug", 10, 20, 0.9),
                 cue("face", 12, 18, 0.9, event="appearance")])
    assert len(anns) == 2


def test_nearby_but_disjoint_ranges_merge_within_gap():
    anns = fuse([cue("scorebug", 10, 20, 0.9), cue("face", 21.5, 25, 0.6)])
    assert len(anns) == 1
    assert anns[0].end_s == 25


def test_far_ranges_stay_separate():
    anns = fuse([cue("scorebug", 10, 20, 0.9), cue("scorebug", 60, 70, 0.9)])
    assert len(anns) == 2


def test_min_confidence_floor():
    anns = fuse([cue("jersey", 10, 12, 0.3)], min_confidence=0.5)
    assert anns == []


def test_output_sorted_by_time():
    anns = fuse([cue("scorebug", 60, 70, 0.9), cue("scorebug", 10, 20, 0.9)])
    assert [a.start_s for a in anns] == [10, 60]


# --- entity-free text cues (overlay readers) --------------------------------

STORY_A = "Европейские лидеры обсуждают создание буферной зоны на линии фронта"
STORY_B = "Умер советский и российский композитор Родион Щедрин сегодня утром"


def text_cue(source, start, end, text, conf=0.9, event="ticker"):
    return cue(source, start, end, conf, event=event, entities=(), text=text)


def test_consecutive_ticker_passages_are_not_one_claim():
    """The entity test only REJECTS when both sides name someone and the names
    disagree, so two cues that name nobody skipped it and fell through to the
    time test. Every ticker passage shares an event, names nobody, and sits
    next to the previous one, so they all merged: on 90s of TV Rain that turned
    nine cues into four, one spanning 58 seconds and five separate stories,
    with `_merge` keeping the longest text and dropping the rest."""
    anns = fuse([
        text_cue("news", 0.0, 10.0, STORY_A),
        text_cue("news", 10.0, 20.0, STORY_B),
    ])
    assert len(anns) == 2
    assert {a.text for a in anns} == {STORY_A, STORY_B}


def test_a_run_of_distinct_passages_does_not_chain_into_one_span():
    """The group's end grows as cues join it, so without a text test the merge
    chains: each passage is within the gap of the one before, and a whole
    file's ticker becomes a single annotation."""
    spans = [(0.0, 10.0), (10.0, 20.0), (20.0, 30.0), (30.0, 40.0)]
    stories = [STORY_A, STORY_B, STORY_A + " сегодня", STORY_B + " в Москве"]
    anns = fuse([text_cue("news", s, e, t) for (s, e), t in zip(spans, stories)])
    assert len(anns) == 4
    assert max(a.end_s - a.start_s for a in anns) == 10.0


def test_two_readings_of_the_same_passage_still_merge():
    """The point of fusion, which the text test must not break: two sources
    that saw the same overlay are one claim, and their confidences combine."""
    (ann,) = fuse([
        text_cue("news", 10.0, 20.0, STORY_A, conf=0.6),
        text_cue("other", 11.0, 21.0, "лидеры обсуждают создание буферной зоны", conf=0.5),
    ])
    assert ann.sources == ("news", "other")
    assert ann.confidence == 0.8  # noisy-OR, as for any two agreeing sources
    assert ann.start_s == 10.0 and ann.end_s == 21.0


def test_cues_that_name_someone_are_unaffected_by_the_text_test():
    """The baseball cascade resolves a roster on every cue, so the text test
    must never reach it: two sources describing one pitch in different words
    are still one claim."""
    anns = fuse([
        cue("playbyplay", 100.0, 108.0, 0.98, text="Pitch: Logan Webb to X"),
        cue("scorebug", 99.0, 110.0, 0.9, text="WEBB"),
    ])
    assert len(anns) == 1


def test_text_free_cues_merge_as_they_always_did():
    """Nothing to compare, so the time and event rules decide alone."""
    anns = fuse([
        text_cue("news", 0.0, 10.0, ""),
        text_cue("news", 10.0, 20.0, ""),
    ])
    assert len(anns) == 1
