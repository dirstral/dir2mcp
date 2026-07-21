"""Command-line entrypoints.

    dirstral-annotate annotate game7.mp4 --roster roster.json \
        [--game-pk 745123 --anchor "PITCH_EPOCH=VIDEO_S" ...] \
        [--faces bank/] [--scorebug] [--jersey] [--fps 0.5] [--min-confidence 0.3]

    dirstral-annotate eval game7.mp4 --roster roster.json --game-pk 745123 \
        --anchor "…=…" [--feed saved-gumbo.json] [--report report.md]

`annotate` writes `game7.vtt` (the v0 sidecar dir2mcp indexes today) and
`game7.annotations.json` (the v1 draft) next to the media file. `eval`
re-runs annotation and scores it against the game's ground truth.

Recognizers whose backends are missing are reported and skipped — the
cascade degrades, it doesn't abort.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .emit import write_sidecars
from .eval import align, ground_truth, report, score
from .fusion import fuse
from .model import Cue, Document
from .recognizers.base import RecognizerUnavailable
from .roster import Roster


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="dirstral-annotate")
    sub = p.add_subparsers(dest="command", required=True)
    for name in ("annotate", "eval"):
        c = sub.add_parser(name)
        c.add_argument("media", type=Path)
        c.add_argument("--roster", type=Path, required=True)
        c.add_argument("--game-pk", type=int, help="MLB statsapi gamePk for play-by-play labels")
        c.add_argument("--feed", type=Path, help="saved GUMBO JSON (skips the network fetch)")
        c.add_argument(
            "--anchor", action="append", default=[], metavar="EPOCH=VIDEO_S",
            help="wall-clock epoch seconds = video seconds; repeatable",
        )
        c.add_argument("--faces", type=Path, help="roster image bank dir; enables face recognition")
        c.add_argument("--scorebug", action="store_true", help="enable scorebug OCR")
        c.add_argument("--jersey", action="store_true", help="enable jersey-number OCR")
        c.add_argument("--fps", type=float, default=0.5, help="frame sampling rate (default 0.5)")
        c.add_argument("--min-confidence", type=float, default=0.0)
    sub.choices["eval"].add_argument("--report", type=Path, help="write markdown report here")
    return p


def _parse_anchors(specs: list[str]) -> list[align.Anchor]:
    anchors = []
    for spec in specs:
        try:
            epoch, video = spec.split("=", 1)
            anchors.append(align.Anchor(epoch_s=float(epoch), video_s=float(video)))
        except ValueError:
            raise SystemExit(f"bad --anchor {spec!r}: expected EPOCH_SECONDS=VIDEO_SECONDS")
    return anchors


def _load_events(args) -> list[ground_truth.PitchEvent]:
    feed = ground_truth.load_game(args.feed) if args.feed else ground_truth.fetch_game(args.game_pk)
    return ground_truth.parse_pitches(feed)


def _collect_cues(args, roster: Roster, alignment: align.Alignment | None) -> list[Cue]:
    cues: list[Cue] = []
    skipped: list[str] = []

    if alignment is not None and (args.game_pk or args.feed):
        from .recognizers.playbyplay import PlayByPlayRecognizer

        events = _load_events(args)
        cues += PlayByPlayRecognizer(events, alignment.offset_s, roster).recognize(args.media)

    def try_recognizer(build):
        try:
            cues.extend(build().recognize(args.media))
        except RecognizerUnavailable as exc:
            skipped.append(str(exc))

    if args.scorebug:
        from .recognizers.scorebug import ScorebugRecognizer

        try_recognizer(lambda: ScorebugRecognizer(roster, fps=args.fps))
    if args.jersey:
        from .recognizers.jersey import JerseyRecognizer

        try_recognizer(lambda: JerseyRecognizer(roster, fps=args.fps))
    if args.faces:
        from .recognizers.faces import FaceRecognizer

        try_recognizer(lambda: FaceRecognizer(roster, args.faces, fps=args.fps))

    for msg in skipped:
        print(f"skipped: {msg}", file=sys.stderr)
    return cues


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    roster = Roster.load(args.roster)
    anchors = _parse_anchors(args.anchor)
    alignment = align.estimate(anchors) if anchors else None

    if (args.game_pk or args.feed) and alignment is None:
        raise SystemExit("play-by-play labels need at least one --anchor to place them on the video timeline")
    if alignment is not None and alignment.drifty:
        print(
            f"warning: anchors disagree by {alignment.spread_s:.1f}s — "
            "timeline may be spliced; results near splices will be off",
            file=sys.stderr,
        )

    cues = _collect_cues(args, roster, alignment)
    annotations = fuse(cues, min_confidence=args.min_confidence)
    doc = Document(media=args.media.name, annotations=annotations)

    if args.command == "annotate":
        written = write_sidecars(doc, args.media, roster)
        print(f"{len(annotations)} annotations from {len(cues)} cues -> "
              + ", ".join(str(w) for w in written))
        return 0

    # eval
    if not (args.game_pk or args.feed):
        raise SystemExit("eval needs --game-pk or --feed for ground truth")
    events = _load_events(args)
    card = score.score(annotations, events, alignment, roster)
    text = report.render(card, alignment, roster, title=args.media.name)
    if args.report:
        args.report.write_text(text, encoding="utf-8")
        print(f"wrote {args.report}")
    else:
        print(text)
    recall = card.overall.recall
    precision = card.overall.precision
    ok = recall is not None and recall >= 0.90 and precision is not None and precision >= 0.95
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
