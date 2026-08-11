"""Command-line entrypoints.

    # Run the recognition backend dir2mcp connects to
    # (dir2mcp config: recognize.provider=serve, recognize.base_url=http://127.0.0.1:8765)
    dirstral-annotate serve --roster roster.json [--games games.json] \
        [--scorebug] [--jersey] [--faces bank/] [--news] [--ocr-lang rus] \
        [--fps 0.5] [--min-confidence 0.3] [--host 127.0.0.1] [--port 8765]

    # Scorebug pitch counts: more pitches found, some of them invented.
    # Opt-in, because it spends precision (see ScorebugRecognizer).
    dirstral-annotate serve --roster roster.json --scorebug --scorebug-pitch-counts

    # A news archive resolves no roster, so it needs none:
    dirstral-annotate serve --news --ocr-lang rus

    # News overlay cues pass a readability gate before they are emitted.
    # It is on by default. This turns it off and returns every cue:
    dirstral-annotate serve --news --news-min-chars 0 --news-min-agreement 0

    # Phase-1 accuracy gate against Statcast ground truth
    dirstral-annotate eval game7.mp4 --roster roster.json \
        (--game-pk 745123 | --feed saved-gumbo.json) --anchor "EPOCH=VIDEO_S" \
        [--report report.md] [--debug]

The report always carries per-source diagnostics (cues emitted, cues surviving
fusion and the confidence floor, near-miss distances) so a recognizer that
contributes nothing to the pitcher-keyed metric can be told apart from one the
metric cannot read; `--debug` adds sample cues per source.

`games.json` binds media basenames to their play-by-play source and time
anchors: {"game7.mp4": {"game_pk": 745123, "anchors": ["1789265400.0=60.0"]}}.
Vision recognizers whose backends are missing are reported and skipped —
the cascade degrades, it doesn't abort.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .eval import diagnose as diagnose_mod
from .eval import report as report_mod
from .eval import score as score_mod
from .pipeline import GameConfig, Pipeline, load_games
from .roster import Roster
from .serve import serve


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="dirstral-annotate")
    sub = p.add_subparsers(dest="command", required=True)

    def vision_flags(c):
        c.add_argument("--scorebug", action="store_true", help="enable scorebug OCR")
        c.add_argument("--scorebug-pitch-counts", action="store_true",
                       help="also read a pitch from each +1 of the bug's pitch count "
                            "(more recall, less precision; needs --scorebug)")
        c.add_argument("--jersey", action="store_true", help="enable jersey-number OCR")
        c.add_argument("--faces", type=Path, help="roster image bank dir; enables face recognition")
        c.add_argument("--news", action="store_true",
                       help="enable news overlay text (headline banner + ticker); needs no roster")
        c.add_argument("--news-min-chars", type=int, metavar="N",
                       help="drop news overlay cues shorter than N characters "
                            "(default 20; 0 disables this half of the gate)")
        c.add_argument("--news-min-agreement", type=float, metavar="F",
                       help="drop news overlay cues read with less agreement than F "
                            "(default 0.6; 0 disables this half of the gate)")
        c.add_argument("--ocr-lang", help="OCR language for the overlay readers (e.g. rus)")
        c.add_argument("--fps", type=float, default=0.5, help="frame sampling rate (default 0.5)")
        c.add_argument("--min-confidence", type=float, default=0.0)

    s = sub.add_parser("serve", help="run the recognition backend for dir2mcp")
    s.add_argument("--roster", type=Path,
                   help="entity vocabulary; required unless the only recognizer is --news")
    s.add_argument("--games", type=Path, help="games.json: media basename -> play-by-play binding")
    s.add_argument("--host", default="127.0.0.1")
    s.add_argument("--port", type=int, default=8765)
    vision_flags(s)

    e = sub.add_parser("eval", help="score recognition against Statcast ground truth")
    e.add_argument("media", type=Path)
    e.add_argument("--roster", type=Path, required=True)
    e.add_argument("--game-pk", type=int)
    e.add_argument("--feed", type=Path, help="saved GUMBO JSON (skips the network fetch)")
    e.add_argument("--anchor", action="append", default=[], metavar="EPOCH=VIDEO_S", required=True)
    e.add_argument("--report", type=Path, help="write markdown report here")
    e.add_argument(
        "--vision-only",
        action="store_true",
        help="score the vision cascade alone: the feed supplies ground truth but "
             "does NOT run as a recognizer",
    )
    e.add_argument(
        "--debug",
        action="store_true",
        help="add per-source sample cues to the report's diagnostics",
    )
    vision_flags(e)
    return p


def _needs_roster(args) -> bool:
    """Whether any enabled recognizer resolves names against a roster.

    `eval` always does: it scores against a feed whose identities are roster
    ids, so a run without one would report zero recall and look like a
    recognition failure rather than a missing argument.
    """
    if args.command != "serve":
        return True
    return bool(args.scorebug or args.jersey or args.faces or args.games)


def _pipeline(args, roster: Roster, games) -> Pipeline:
    return Pipeline(
        roster=roster,
        games=games,
        scorebug=args.scorebug,
        scorebug_pitch_counts=args.scorebug_pitch_counts,
        jersey=args.jersey,
        news=args.news,
        news_min_chars=args.news_min_chars,
        news_min_agreement=args.news_min_agreement,
        ocr_lang=args.ocr_lang,
        faces_bank=args.faces,
        fps=args.fps,
        min_confidence=args.min_confidence,
    )


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    # A news archive has no roster, and inventing an empty one for the
    # recognizers that DO read one would silently resolve nobody rather than
    # say why. So the roster is optional exactly when nothing needs it.
    if args.roster is None:
        if _needs_roster(args):
            raise SystemExit(
                "--roster is required for --scorebug, --jersey, --faces and "
                "play-by-play; only --news reads no roster"
            )
        roster = Roster([])
    else:
        roster = Roster.load(args.roster)

    if args.command == "serve":
        games = load_games(args.games) if args.games else {}
        server = serve(_pipeline(args, roster, games), host=args.host, port=args.port)
        print(f"recognition backend listening on http://{args.host}:{server.server_address[1]} "
              f"({len(games)} game binding(s))")
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            server.shutdown()
        return 0

    # eval
    if not (args.game_pk or args.feed):
        raise SystemExit("eval needs --game-pk or --feed for ground truth")
    game = GameConfig.parse({
        "game_pk": args.game_pk,
        "feed": str(args.feed) if args.feed else None,
        "anchors": args.anchor,
    })
    if not game.anchors:
        raise SystemExit("eval needs at least one --anchor to place labels on the video timeline")
    alignment = game.alignment()
    if alignment.drifty:
        print(
            f"warning: anchors disagree by {alignment.spread_s:.1f}s — "
            "timeline may be spliced; results near splices will be off",
            file=sys.stderr,
        )

    # `--vision-only` is the feed-free measurement (#741 milestone 1). The feed
    # still supplies ground truth, which is what a label set IS, but it is kept
    # out of the pipeline so play-by-play does not run as a recognizer.
    #
    # Without this the headline is mostly a measurement of wall-clock alignment:
    # play-by-play emits a `pitch` cue per feed event, the scorer reads `pitch`
    # annotations, and the feed is therefore scored against itself. That is the
    # right number for a corpus that HAS a feed and a misleading one for the
    # archives this milestone targets.
    bindings = {} if args.vision_only else {args.media.name: game}
    pipeline = _pipeline(args, roster, bindings)
    if args.vision_only:
        print("vision-only: play-by-play supplies ground truth only, not cues",
              file=sys.stderr)
    # Keep the pre-fusion cues: they are what makes an empty source row
    # explainable (ineligible / floored / weak) instead of just empty.
    cues, annotations = diagnose_mod.run_pipeline(pipeline, args.media)
    for msg in pipeline.skipped:
        print(f"skipped: {msg}", file=sys.stderr)

    events = game.events()
    card = score_mod.score(annotations, events, alignment, roster)
    diagnostics = diagnose_mod.diagnose(
        cues, annotations, events, alignment, roster, card,
        min_confidence=args.min_confidence,
    )
    text = report_mod.render(
        card, alignment, roster, title=args.media.name,
        diagnostics=diagnostics, debug=args.debug,
    )
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
