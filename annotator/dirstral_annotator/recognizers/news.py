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

## Readable enough to cite

Agreement decides which BAND holds an overlay. It does not decide whether the
words that came off that band are words. A fixed graphic inside the band is
real pixels, so both passes misread it the same way and it scores as high as a
clean read. The output is therefore citable text of which most is noise, and a
citation pointing at noise is worse than no citation at all.

So an emitted cue passes a second gate, on the cue rather than on the band.
See `READABLE_MIN_CHARS` for the measurement that chose it. Two properties of
where the gate sits matter:

* It runs AFTER the collapse, on the cue, because that is what was measured
  and what a consumer cites. A per-frame gate would also change which band
  locks, since `interpret` supplies the band search its evidence: a short read
  is still evidence that this band carries an overlay, and rejecting it there
  would move the lock and change what gets READ, not just what gets emitted.
* It drops the cue instead of lowering its confidence. Confidence here means
  agreement between the passes and nothing else, and `fusion.fuse` combines
  confidences under noisy-OR, so two rejected cues of one passage would
  compound back into a high-confidence annotation. A floor that is not applied
  is also no floor: `min_confidence` defaults to 0.0.

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
    "READABLE_MIN_AGREEMENT",
    "READABLE_MIN_CHARS",
    "TICKER_REGIONS",
    "NewsOverlayRecognizer",
    "OverlayRole",
    "is_readable",
    "read_agreement",
]

#: Minimum agreement between the two preprocessing passes for a band read to
#: count as overlay text. See the module docstring for the measurement; the
#: background population sat at 0.00, so this is a floor with room under it
#: rather than a boundary between two touching distributions.
DEFAULT_AGREEMENT = 0.3

#: The readability gate on an emitted cue: how many characters its text must
#: carry, and how firmly that text must have been read. Both floors apply, and
#: either one set to 0 switches its half of the gate off.
#:
#: Measured on 145 cues (94 headline, 51 ticker) from 15 minutes of a TV Rain
#: broadcast at 720p, read with tesseract `rus`. A cue counted as readable when
#: it held a word of 4 or more Cyrillic letters that the programme's own
#: subtitle track also held, so the vocabulary came from the same broadcast and
#: needed no external dictionary. Garbled text almost never lands on a real
#: word by accident.
#:
#:   | gate                             | keeps | precision | recall |
#:   |----------------------------------|-------|-----------|--------|
#:   | none                             |   145 |     40.0% | 100.0% |
#:   | agreement >= 0.6                 |    60 |     60.0% |  62.1% |
#:   | chars >= 20                      |    87 |     65.5% |  98.3% |
#:   | chars >= 20 and agreement >= 0.6 |    40 |     90.0% |  62.1% |
#:   | chars >= 25 and agreement >= 0.6 |    36 |     91.7% |  56.9% |
#:
#: Neither signal works alone. Agreement alone is flat, not weak: a floor of
#: 0.5 gives 48.5% precision and a floor of 1.0 gives 48.8%, while recall falls
#: from 81.0% to 34.5%. Length alone is the stronger single signal, because the
#: garbage is short (median 15 characters against 32), but it tops out near
#: 68%. Together they take precision from 40% to 90%, and that is the point:
#: agreement carries real information once the short fragments are gone.
#:
#: 20 and 0.6 is the knee. Moving the length floor to 25 buys 1.7 points of
#: precision for 5.2 points of recall.
#:
#: Measured on the same cues and REJECTED: vowel fraction (the garbage has MORE
#: vowels, 0.50 against 0.44, so it separates the wrong way), Cyrillic fraction
#: (1.00 for both, because the reader is pinned to one script), and character
#: bigram plausibility (the medians separate but the ranges overlap, and it
#: degenerates on short text, which is exactly where the garbage is).
#:
#: One corpus is one corpus, so both floors are parameters. See #751.
READABLE_MIN_CHARS = 20
READABLE_MIN_AGREEMENT = 0.6

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


def is_readable(
    cue: Cue,
    *,
    min_chars: int = READABLE_MIN_CHARS,
    min_agreement: float = READABLE_MIN_AGREEMENT,
) -> bool:
    """Whether a cue carries words rather than misread pixels.

    Both floors have to hold. Each one alone admits most of the garbage; see
    `READABLE_MIN_CHARS` for the numbers. The cue's confidence IS the run's
    agreement, so this is the same quantity the band floor uses, applied a
    second time at a higher level and a higher threshold.
    """
    return len(cue.text.strip()) >= min_chars and cue.confidence >= min_agreement


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
    #: How many collapsed cues the readability gate dropped on the last run.
    #: Without it an empty result is ambiguous: a role that found no overlay
    #: and a role that found one and could not read it look the same, and they
    #: are different answers, exactly as `answered` exists to say.
    rejected: int = 0

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

    A cue that fails the readability gate is not emitted. The gate is ON by
    default, because the ungated output measured 40% precision and the product
    of this cascade is citations: a cited span whose text is noise costs more
    than a passage never found. `readable_chars=0, readable_agreement=0.0`
    turns it off and returns the ungated stream.
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
        readable_chars: int = READABLE_MIN_CHARS,
        readable_agreement: float = READABLE_MIN_AGREEMENT,
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
        if readable_chars < 0:
            raise ValueError(f"readable_chars must not be negative: {readable_chars}")
        if not 0.0 <= readable_agreement <= 1.0:
            raise ValueError(
                f"readability agreement floor out of [0,1]: {readable_agreement}"
            )
        # Checked here rather than at the first division: every timing this
        # class derives is 1/fps or a multiple, so a zero raises deep inside a
        # collapse and a negative one quietly produces cues that end before
        # they start.
        if fps <= 0:
            raise ValueError(f"fps must be positive: {fps}")
        self.fps = fps
        self.agreement = agreement
        self.readable_chars = readable_chars
        self.readable_agreement = readable_agreement
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
            role_reader.rejected = 0
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
        cues = collapse_text_sightings(
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
        # The gate goes here, on the whole run's cue, because the run is what
        # was measured and what a consumer cites. It cannot go in `interpret`:
        # that return value is the band search's evidence, so rejecting a short
        # read there would move the lock and change which band gets read.
        keep = [
            cue
            for cue in cues
            if is_readable(
                cue,
                min_chars=self.readable_chars,
                min_agreement=self.readable_agreement,
            )
        ]
        role_reader.rejected = len(cues) - len(keep)
        return keep

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

    def rejected_counts(self) -> dict[str, int]:
        """How many cues per role the readability gate dropped, after a `recognize`.

        A role that answered from a band and still emitted nothing found an
        overlay it could not read, which is a different answer from finding no
        overlay. Zero until a `recognize` has run.
        """
        return {rr.role.event: rr.rejected for rr in self._readers}


def roles_from(spec: Sequence[tuple[str, Sequence[Region]]]) -> tuple[OverlayRole, ...]:
    """Build roles from a plain (event, regions) sequence, for config loading."""
    return tuple(
        OverlayRole(event=event, regions=tuple(regions)) for event, regions in spec
    )
