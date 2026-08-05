"""Read burned-in news overlays into time-anchored text cues.

The corpus-side counterpart of `scorebug.py`. Both drive the generic reader in
`overlay.py`; what differs is what the text is taken to MEAN. Baseball resolves
it against a roster and emits identities. A news broadcast has no roster and no
feed, so the text is the payload: a headline banner, a scrolling ticker, a
segment title. The cue carries the words, and `#741 milestone 3` is about
getting those words onto a timeline something can cite.

## What counts as evidence

`overlay.py` warns that the interpreter is not optional decoration: it supplies
the hit count that drives the band lock, so a reader with a weak interpreter
locks onto the crowd. Baseball has a strong signal for free, because a roster
either recognises a name or it does not. Here there is no vocabulary to check
against, and "the OCR returned something" is true of studio furniture, a
bookshelf behind a presenter, and a wall of moire.

The signal used instead is **agreement between the two preprocessing passes**.
`_prepared_crops` renders each band twice, greyscale and hard-thresholded, and
those are two largely independent views of the same pixels. Real glyphs survive
both. Noise is a property of the rendering, so it does not: the passes invent
different garbage and their texts have nothing in common.

Measured on 105 band reads from 15 frames of a TV Rain news broadcast, OCR'd
with tesseract `rus`, comparing the two overlay bands against five background
bands per frame:

  | population              | median agreement | worst |
  |-------------------------|------------------|-------|
  | overlay (30 reads)      | 0.70             | 0.20  |
  | background (75 reads)   | 0.00             | 0.00  |

Not one background band produced ANY agreement, which is why the default floor
is low: at 0.3 this keeps 93% of overlay reads and admits none of the 75
background ones. The separation is wide enough that the exact number is not
delicate, and it is a parameter regardless, because one corpus is one corpus.

The reads it loses are all one all-caps display face whose letterforms OCR
inconsistently between the two renderings. They are lost, not misread: a
dropped frame of a ticker costs nothing, because the passage is still on screen
in the next one and `collapse_text_sightings` joins the run across the gap.

## One reader per role

`_RegionSearch` locks a single band and `OverlayReader.read` stops a frame once
a band answers, which is right when there is one overlay to find. A news frame
carries several at once: a headline banner and a ticker occupy different bands
and say different things, and a clock badge occupies a third (that one is
`clock.py`, which has its own much stronger evidence in a parseable time next
to a named zone).

So each role gets its own reader over its own candidate bands. The frame
extraction is shared, because `iter_frames` memoises per (media, fps), so the
extra cost is OCR on a second band rather than a second decode of the file.
"""

from __future__ import annotations

from collections.abc import Iterable, Sequence
from contextlib import closing
from dataclasses import dataclass, field
from pathlib import Path

from ..model import Cue
from .base import collapse_text_sightings, text_overlap, text_tokens
from .overlay import SCAN_STRIDE, OcrFn, OverlayRead, OverlayReader, Region

__all__ = [
    "DEFAULT_AGREEMENT",
    "HEADLINE_REGIONS",
    "NEWS_ROLES",
    "TICKER_REGIONS",
    "NewsOverlayRecognizer",
    "OverlayRole",
    "read_agreement",
]

#: Minimum agreement between the two preprocessing passes for a band read to
#: count as overlay text. See the module docstring for the measurement; the
#: background population sat at 0.00, so this is a floor with room under it
#: rather than a boundary between two touching distributions.
DEFAULT_AGREEMENT = 0.3

#: Candidate bands for a headline banner, and for a ticker along the bottom.
#: Broadcast convention rather than one broadcaster's layout, in the spirit of
#: `CLOCK_REGIONS`: a lower third sits in the bottom quarter and a ticker below
#: it. Kept disjoint so two readers over one frame cannot both lock the same
#: band and report it twice under different names.
HEADLINE_REGIONS: tuple[Region, ...] = (
    (0.0, 0.80, 1.0, 0.10),
    (0.0, 0.70, 1.0, 0.10),
)
TICKER_REGIONS: tuple[Region, ...] = (
    (0.0, 0.91, 1.0, 0.09),
    (0.0, 0.90, 1.0, 0.07),
)


@dataclass(frozen=True)
class OverlayRole:
    """One kind of overlay to look for, and where to look for it.

    `event` names the cue, so it is what a downstream consumer filters on. The
    regions are candidates, not a location: the reader sweeps them and locks
    whichever proves itself, exactly as it does for a scorebug.
    """

    event: str
    regions: tuple[Region, ...]

    def __post_init__(self) -> None:
        if not self.event.strip():
            raise ValueError("an overlay role needs an event name")
        if not self.regions:
            raise ValueError(f"role {self.event!r} has no candidate regions")


#: The roles a news broadcast usually carries. A caller with a different
#: graphics package passes its own; nothing below assumes these two.
NEWS_ROLES: tuple[OverlayRole, ...] = (
    OverlayRole(event="headline", regions=HEADLINE_REGIONS),
    OverlayRole(event="ticker", regions=TICKER_REGIONS),
)


def read_agreement(read: OverlayRead) -> float:
    """How much the preprocessing passes agree about what this band says.

    0.0 when either pass found no tokens, which is the ordinary shape of a
    background band: one rendering yields nothing and the other yields noise.
    """
    texts = [text for text in read.texts if text.strip()]
    if len(texts) < 2:
        return 0.0
    # Pairwise best rather than first-against-second, so the measure does not
    # depend on how many passes `_prepared_crops` happens to yield.
    return max(
        text_overlap(texts[i], texts[j])
        for i in range(len(texts))
        for j in range(i + 1, len(texts))
    )


def _fullest(read: OverlayRead) -> str:
    """The pass that recovered the most words.

    By token count, not character count: a pass that dissolves a line into
    punctuation can be the longest string while carrying the least text.
    """
    return max(read.texts, key=lambda text: len(text_tokens(text)), default="")


@dataclass
class _RoleReader:
    """One role's reader and the sightings it has produced."""

    role: OverlayRole
    reader: OverlayReader
    agreement: float
    sightings: list[tuple[float, str, float]] = field(default_factory=list)
    #: The band this role's reads actually came from. The reader cannot report
    #: it: the lock lives in a `_RegionSearch` built per `read()` call and
    #: discarded with the generator, so the interpreter is the only place that
    #: sees which region answered.
    answered: Region | None = None

    def interpret(self, read: OverlayRead) -> tuple[str, int]:
        """Return this band's text and whether it counts as evidence.

        The hit is all-or-nothing rather than proportional to the text: a long
        noisy read must not outvote a short clean one in the band search, and
        length is exactly what a band of background texture has plenty of.
        """
        agreed = read_agreement(read)
        if agreed < self.agreement:
            return "", 0
        text = _fullest(read).strip()
        if not text_tokens(text):
            return "", 0
        self.sightings.append((read.timestamp_s, text, round(min(agreed, 1.0), 4)))
        self.answered = read.region
        return text, 1


class NewsOverlayRecognizer:
    """Turn a broadcast's burned-in overlays into citable text cues.

    Emits one cue per passage per role: a headline that stays up for twenty
    seconds is one cue, and a ticker that scrolls through five stories is five,
    because `collapse_text_sightings` joins reads that are still the same
    passage and starts a new run when the words have turned over.

    Confidence is the agreement between the preprocessing passes, which is a
    statement about how firmly the text was read and not about whether it is
    true. Nothing here resolves an entity, so cues carry no `entity_ids`.
    """

    name = "news"

    def __init__(
        self,
        *,
        roles: Iterable[OverlayRole] = NEWS_ROLES,
        lang: str | None = None,
        psm: int | None = None,
        fps: float = 0.5,
        agreement: float = DEFAULT_AGREEMENT,
        workers: int | None = None,
        ocr: OcrFn | None = None,
        similarity: float | None = None,
        frame_gap: float | None = None,
    ) -> None:
        roles = tuple(roles)
        if not roles:
            raise ValueError("a news recognizer needs at least one overlay role")
        if not 0.0 <= agreement <= 1.0:
            raise ValueError(f"agreement floor out of [0,1]: {agreement}")
        # Checked here rather than at the first division: every timing this
        # class derives is 1/fps or a multiple, so a zero raises deep inside a
        # collapse and a negative one quietly produces cues that end before
        # they start.
        if fps <= 0:
            raise ValueError(f"fps must be positive: {fps}")
        self.fps = fps
        self.agreement = agreement
        self.similarity = similarity
        self.roles = roles
        # The collapse gap has to be at least the BAND SEARCH's sampling
        # interval, not the frame interval. Until a band locks, `_RegionSearch`
        # only offers crops every SCAN_STRIDE frames, so at fps 0.5 the first
        # reads of a file arrive 8s apart by design. A gap of 1/fps treats each
        # of them as its own run and a banner that never moved comes back as
        # five two-second cues instead of one, which is what it did before this
        # was set: the reads are sparse, not the overlay.
        self.frame_gap = frame_gap if frame_gap is not None else SCAN_STRIDE / fps
        # `psm` is only forwarded when the caller set one: OverlayReader
        # defaults it to OCR_PSM, and passing None through would silently swap
        # that for the engine's own default.
        psm_kwargs = {} if psm is None else {"psm": psm}
        self._readers = [
            _RoleReader(
                role=role,
                reader=OverlayReader(
                    ocr=ocr,
                    lang=lang,
                    fps=fps,
                    regions=role.regions,
                    workers=workers,
                    name=f"{self.name}-{role.event}",
                    **psm_kwargs,
                ),
                agreement=agreement,
            )
            for role in roles
        ]

    def recognize(self, media_path: Path) -> list[Cue]:
        cues: list[Cue] = []
        for role_reader in self._readers:
            # Both, not just the sightings: a recognizer reused on a second
            # file with no overlay would otherwise still report the band the
            # PREVIOUS file answered from.
            role_reader.sightings.clear()
            role_reader.answered = None
            # `closing` because the reader owns a worker pool and a scratch
            # directory for the length of the iteration: abandoning it part way
            # through has to shut them down now, not at collection.
            with closing(
                role_reader.reader.read(media_path, role_reader.interpret)
            ) as reads:
                for _ in reads:
                    pass
            cues += self._collapse(role_reader)
        cues.sort(key=lambda cue: (cue.start_s, cue.event))
        return cues

    def _collapse(self, role_reader: _RoleReader) -> list[Cue]:
        extra = {} if self.similarity is None else {"similarity": self.similarity}
        return collapse_text_sightings(
            role_reader.sightings,
            source=self.name,
            event=role_reader.role.event,
            frame_gap=self.frame_gap,
            # A cue ends one FRAME after its last read, not one search stride:
            # widening both would run every ticker cue past the start of the
            # next headline and produce overlapping citations.
            cue_gap=1.0 / self.fps,
            **extra,
        )

    @property
    def regions(self) -> tuple[Region, ...]:
        """Every candidate band across every role, for callers that report them."""
        seen: list[Region] = []
        for role in self.roles:
            for region in role.regions:
                if region not in seen:
                    seen.append(region)
        return tuple(seen)

    def answered_regions(self) -> dict[str, Region | None]:
        """Which band each role's reads came from, after a `recognize`.

        Useful for reporting on an unfamiliar corpus: a role that answered from
        nowhere found no overlay at all, which is a different answer from
        finding one and reading it badly. None until a `recognize` has run.
        """
        return {rr.role.event: rr.answered for rr in self._readers}


def roles_from(spec: Sequence[tuple[str, Sequence[Region]]]) -> tuple[OverlayRole, ...]:
    """Build roles from a plain (event, regions) sequence, for config loading."""
    return tuple(
        OverlayRole(event=event, regions=tuple(regions)) for event, regions in spec
    )
