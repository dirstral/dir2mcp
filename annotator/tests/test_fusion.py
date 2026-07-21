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
