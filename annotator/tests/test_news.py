"""Backend-free tests for the news overlay interpreter.

The reader is driven off a per-band script rather than images and an engine, so
these run anywhere. What they pin is the interpretation: what counts as
evidence that a band holds an overlay, and what shape the cues come out in.
"""

import pytest

from dirstral_annotator.recognizers import news, overlay
from dirstral_annotator.recognizers.news import (
    DEFAULT_AGREEMENT,
    NEWS_ROLES,
    NewsOverlayRecognizer,
    OverlayRole,
    read_agreement,
)
from dirstral_annotator.recognizers.overlay import OverlayRead

MEDIA = __import__("pathlib").Path("broadcast.mp4")
HEADLINE_BAND = (0.0, 0.80, 1.0, 0.10)
TICKER_BAND = (0.0, 0.91, 1.0, 0.09)
BACKGROUND = (0.0, 0.30, 1.0, 0.10)

HEADLINE = "«ЯНДЕКС» ОШТРАФОВАЛИ ЗА ОТКАЗ ПРЕДОСТАВИТЬ ФСБ ДОСТУП К «АЛИСЕ»"


def read(*texts, region=HEADLINE_BAND, t=0.0):
    return OverlayRead(index=0, timestamp_s=t, region=region, texts=tuple(texts))


# --- what counts as evidence ------------------------------------------------

def test_passes_that_agree_are_evidence():
    """Real glyphs survive both renderings, so the two passes say the same
    thing. This is the whole signal, since there is no roster to check against."""
    assert read_agreement(read(HEADLINE, HEADLINE)) == 1.0
    assert read_agreement(read(HEADLINE, HEADLINE[:40])) >= 0.5


def test_a_band_only_one_pass_could_read_is_not_evidence():
    """The ordinary shape of a background band: one rendering yields nothing
    and the other yields noise. Nothing corroborates anything."""
    assert read_agreement(read("", "какой-то шум")) == 0.0
    assert read_agreement(read(HEADLINE, "")) == 0.0
    assert read_agreement(read(HEADLINE)) == 0.0


def test_passes_that_invent_different_garbage_are_not_evidence():
    """Noise is a property of the rendering, so the two passes do not agree on
    it. Measured on 75 background bands of real footage: agreement 0.00 on every
    single one."""
    assert read_agreement(read("ЕЕК ЕЕЕ НЕЕ", "ы:евм шсп 1=")) < DEFAULT_AGREEMENT


# --- roles ------------------------------------------------------------------

def test_a_role_needs_a_name_and_somewhere_to_look():
    with pytest.raises(ValueError):
        OverlayRole(event="", regions=(HEADLINE_BAND,))
    with pytest.raises(ValueError):
        OverlayRole(event="headline", regions=())


def test_the_recognizer_refuses_an_impossible_agreement_floor():
    with pytest.raises(ValueError):
        NewsOverlayRecognizer(agreement=1.5)
    with pytest.raises(ValueError):
        NewsOverlayRecognizer(roles=())


# --- reading ----------------------------------------------------------------

@pytest.fixture
def bands(monkeypatch, tmp_path):
    """Script one text per band, per frame index.

    `install({region: text_or_fn})` makes every sampled frame answer with that
    text for that band. A callable receives the frame index, which is how a
    scrolling ticker is expressed.
    """

    def install(script, frames=40, fps=0.5):
        listing = []
        for i in range(frames):
            path = tmp_path / f"frame-{i:05d}.jpg"
            path.write_text(str(i))
            listing.append((i / fps, path))
        index = {}

        def read_band(ocr, frame, region, work):
            i = index[frame]
            value = script.get(tuple(region), "")
            text = value(i) if callable(value) else value
            # Both preprocessing passes: a scripted band is a legible one, so
            # they agree, which is exactly what the interpreter looks for.
            return [text, text]

        for i, (_, path) in enumerate(listing):
            index[path] = i
        monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(listing))
        monkeypatch.setattr(overlay, "read_band", read_band)
        return listing

    return install


def only(recognizer, event):
    return [c for c in recognizer.recognize(MEDIA) if c.event == event]


def test_a_static_banner_is_one_cue_not_one_per_frame(bands):
    bands({HEADLINE_BAND: HEADLINE})
    cues = only(NewsOverlayRecognizer(workers=1), "headline")
    assert len(cues) == 1
    assert cues[0].text == HEADLINE
    assert cues[0].source == "news"


def test_a_banner_seen_only_during_the_strided_search_is_still_one_cue(bands):
    """The reader samples every SCAN_STRIDE frames until a band locks, so the
    first reads of a file arrive one search stride apart by design. Collapsing
    on the frame interval would call each of them its own sighting and report a
    banner that never moved as a handful of two-second cues."""
    bands({HEADLINE_BAND: HEADLINE})
    cues = only(NewsOverlayRecognizer(workers=1), "headline")
    assert len(cues) == 1
    assert cues[0].start_s == 0.0


def test_a_cue_ends_one_frame_after_its_last_read_not_one_stride(bands):
    """The join tolerance and the trailing extension are different numbers.
    Widening both would run each cue past the start of the next, so consecutive
    ticker headlines would carry overlapping citations."""
    fps, frames = 0.5, 40  # enough to get past the strided search and lock
    bands({HEADLINE_BAND: HEADLINE}, frames=frames, fps=fps)
    (cue,) = only(NewsOverlayRecognizer(workers=1, fps=fps), "headline")
    last_read_s = (frames - 1) / fps
    assert cue.end_s == pytest.approx(last_read_s + 1.0 / fps)
    # The point of the assertion: one frame, not one search stride.
    assert cue.end_s < last_read_s + overlay.SCAN_STRIDE / fps


def test_consecutive_ticker_passages_do_not_overlap(bands):
    """A ticker's cues have to tile: a citation that claims a span belonging to
    the next headline is worse than no citation."""
    stories = [
        "Власти США одобрили продажу Украине ракет увеличенной дальности",
        "Умер советский и российский композитор Родион Щедрин сегодня утром",
        "Европейские лидеры обсуждают создание буферной зоны на линии фронта",
    ]

    def scroll(i):
        return stories[min(i // 14, len(stories) - 1)]

    bands({TICKER_BAND: scroll}, frames=42)
    cues = only(NewsOverlayRecognizer(workers=1), "ticker")
    assert len(cues) >= len(stories)
    for earlier, later in zip(cues, cues[1:]):
        assert earlier.end_s <= later.start_s + 1e-9


def test_two_roles_read_two_bands_and_are_reported_apart(bands):
    """A news frame carries a headline and a ticker at once, saying different
    things. One reader locks one band, so each role gets its own."""
    ticker = "Число пострадавших выросло до сорока человек по данным МЧС"
    bands({HEADLINE_BAND: HEADLINE, TICKER_BAND: ticker})
    recognizer = NewsOverlayRecognizer(workers=1)
    cues = recognizer.recognize(MEDIA)
    events = {c.event for c in cues}
    assert events == {"headline", "ticker"}
    assert any(c.text == HEADLINE for c in cues if c.event == "headline")
    assert any(c.text == ticker for c in cues if c.event == "ticker")
    assert recognizer.answered_regions() == {
        "headline": HEADLINE_BAND,
        "ticker": TICKER_BAND,
    }


def test_footage_with_no_overlay_yields_no_cues(bands):
    """Declining is an answer. Inventing a headline out of studio furniture is
    the failure this interpreter exists to avoid."""
    bands({BACKGROUND: "чтото в кадре"})
    recognizer = NewsOverlayRecognizer(workers=1)
    assert recognizer.recognize(MEDIA) == []
    assert set(recognizer.answered_regions().values()) == {None}


def test_a_band_whose_passes_disagree_is_not_read(monkeypatch, tmp_path):
    """The band search runs on the hit count, so a band that fails the
    agreement floor must not vote for itself either."""
    listing = []
    for i in range(24):
        path = tmp_path / f"frame-{i:05d}.jpg"
        path.write_text(str(i))
        listing.append((i / 0.5, path))
    monkeypatch.setattr(overlay, "iter_frames", lambda *a, **k: iter(listing))
    monkeypatch.setattr(
        overlay,
        "read_band",
        lambda ocr, frame, region, work: ["ЕЕК ЕЕЕ НЕЕ ЕЕЕЕ", "ы:евм шсп 1= ннн"],
    )
    recognizer = NewsOverlayRecognizer(workers=1)
    assert recognizer.recognize(MEDIA) == []


def test_cues_carry_no_entity_ids(bands):
    """Headline text is a topic, not an entity; nothing here resolves a roster."""
    bands({HEADLINE_BAND: HEADLINE})
    for cue in NewsOverlayRecognizer(workers=1).recognize(MEDIA):
        assert cue.entity_ids == ()


def test_confidence_is_how_firmly_the_text_was_read(bands):
    """Agreement between the two renderings, which says nothing about whether
    the headline is true. Identical passes are a perfect read."""
    bands({HEADLINE_BAND: HEADLINE})
    (cue,) = only(NewsOverlayRecognizer(workers=1), "headline")
    assert cue.confidence == 1.0


def test_the_agreement_floor_is_a_parameter(bands):
    """One corpus is one corpus. A caller on different footage moves it."""
    assert news.DEFAULT_AGREEMENT == pytest.approx(0.3)
    bands({HEADLINE_BAND: HEADLINE})
    assert only(NewsOverlayRecognizer(workers=1, agreement=0.99), "headline")


def test_the_default_roles_look_where_broadcasters_put_overlays(bands):
    """Convention, not one broadcaster's layout. The two default bands must
    stay disjoint, or two readers could lock the same band and report it twice
    under different names."""
    assert [r.event for r in NEWS_ROLES] == ["headline", "ticker"]
    for headline_band in news.HEADLINE_REGIONS:
        for ticker_band in news.TICKER_REGIONS:
            h_top, h_bottom = headline_band[1], headline_band[1] + headline_band[3]
            t_top, t_bottom = ticker_band[1], ticker_band[1] + ticker_band[3]
            assert h_bottom <= t_top or t_bottom <= h_top


def test_a_recognizer_reused_on_a_second_file_does_not_report_the_first_ones_band(bands):
    """`answered_regions` says where a role's reads came from. Reporting the
    PREVIOUS file's band for a file with no overlay would turn "found nothing"
    into "found it here", which is the opposite answer."""
    bands({HEADLINE_BAND: HEADLINE})
    recognizer = NewsOverlayRecognizer(workers=1)
    assert recognizer.recognize(MEDIA)
    assert recognizer.answered_regions()["headline"] == HEADLINE_BAND

    bands({BACKGROUND: "чтото в кадре"})
    assert recognizer.recognize(MEDIA) == []
    assert set(recognizer.answered_regions().values()) == {None}


def test_a_non_positive_fps_is_refused_at_construction():
    """Every timing this class derives is 1/fps or a multiple of it, so zero
    raises deep inside a collapse and a negative quietly produces cues that end
    before they start."""
    with pytest.raises(ValueError):
        NewsOverlayRecognizer(fps=0)
    with pytest.raises(ValueError):
        NewsOverlayRecognizer(fps=-0.5)


# --- wiring -----------------------------------------------------------------

def test_the_news_recognizer_needs_no_roster():
    """A news archive has no roster, and every other recognizer in the cascade
    resolves one. Requiring it would make the corpus this milestone targets
    unservable."""
    from dirstral_annotator.cli import _needs_roster, build_parser

    parse = build_parser().parse_args
    assert _needs_roster(parse(["serve", "--news"])) is False
    assert _needs_roster(parse(["serve", "--news", "--scorebug"])) is True
    assert _needs_roster(parse(["serve", "--jersey"])) is True
    assert _needs_roster(parse(["serve", "--games", "g.json"])) is True


def test_the_pipeline_runs_the_news_recognizer_when_asked(monkeypatch, tmp_path):
    """The cascade seam: `--news` has to reach a NewsOverlayRecognizer, with
    the OCR language it was given rather than a constant."""
    from dirstral_annotator import pipeline as pipeline_mod
    from dirstral_annotator.recognizers import news as news_mod
    from dirstral_annotator.roster import Roster

    built = {}

    class Fake:
        def __init__(self, **kwargs):
            built.update(kwargs)

        def recognize(self, media_path):
            return []

    monkeypatch.setattr(news_mod, "NewsOverlayRecognizer", Fake)
    media = tmp_path / "broadcast.mp4"
    media.write_bytes(b"")
    pipeline_mod.Pipeline(
        roster=Roster([]), news=True, ocr_lang="rus", fps=0.25
    ).cues_for(media)
    assert built == {"lang": "rus", "fps": 0.25}
