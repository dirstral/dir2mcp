"""Backend-free unit tests for recognizer logic: run collapsing (shared by
all frame-sampling recognizers) and face gallery matching."""

from dirstral_annotator.recognizers.faces import centroid, cosine, match
from dirstral_annotator.recognizers.scorebug import collapse_sightings


def test_collapse_consecutive_sightings_into_one_cue():
    sightings = [(t, "player:webb-logan", 0.8) for t in (10.0, 12.0, 14.0, 16.0)]
    cues = collapse_sightings(sightings, source="scorebug", event="at_bat", frame_gap=2.0,
                              describe=lambda pid: pid)
    assert len(cues) == 1
    c = cues[0]
    assert c.start_s == 10.0 and c.end_s == 18.0  # extended by one frame interval
    assert c.confidence == 0.8 and c.source == "scorebug"


def test_collapse_survives_ocr_flicker_but_splits_real_gaps():
    sightings = [(10.0, "p", 0.8), (12.0, "p", 0.9), (16.0, "p", 0.7),  # 4s gap: flicker
                 (60.0, "p", 0.8)]  # 44s gap: a new sighting
    cues = collapse_sightings(sightings, source="face", event="appearance", frame_gap=2.0,
                              describe=lambda pid: pid)
    assert len(cues) == 2
    assert cues[0].start_s == 10.0 and cues[0].confidence == 0.9
    assert cues[1].start_s == 60.0


def test_collapse_keeps_players_separate():
    sightings = [(10.0, "a", 0.8), (10.0, "b", 0.8)]
    cues = collapse_sightings(sightings, source="face", event="appearance", frame_gap=2.0,
                              describe=lambda pid: pid)
    assert {c.entity_ids[0] for c in cues} == {"a", "b"}


def test_cosine_and_centroid():
    assert abs(cosine([1, 0], [1, 0]) - 1.0) < 1e-9
    assert abs(cosine([1, 0], [0, 1])) < 1e-9
    assert cosine([0, 0], [1, 0]) == 0.0
    assert centroid([[0, 2], [2, 0]]) == [1, 1]


def test_match_threshold_and_confidence():
    gallery = {"player:webb-logan": [1.0, 0.0], "player:doval-camilo": [0.0, 1.0]}
    hit = match([0.9, 0.1], gallery, threshold=0.4)
    assert hit is not None and hit[0] == "player:webb-logan"
    assert 0.0 < hit[1] <= 1.0
    # equidistant-and-below-threshold gets rejected, not guessed
    assert match([0.1, 0.1], gallery, threshold=0.99) is None
