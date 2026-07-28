"""Command-line entrypoints.

    # Run the recognition backend dir2mcp connects to
    # (dir2mcp config: recognize.provider=serve, recognize.base_url=http://127.0.0.1:8765)
    dirstral-annotate serve --roster roster.json [--games games.json] \
        [--scorebug] [--jersey] [--faces bank/] [--fps 0.5] [--min-confidence 0.3] \
        [--host 127.0.0.1] [--port 8765]

    # Phase-1 accuracy gate against Statcast ground truth
    dirstral-annotate eval game7.mp4 --roster roster.json \
        (--game-pk 745123 | --feed saved-gumbo.json) --anchor "EPOCH=VIDEO_S" \
        [--report report.md]

`games.json` binds media basenames to their play-by-play source and time
anchors: {"game7.mp4": {"game_pk": 745123, "anchors": ["1789265400.0=60.0"]}}.
Vision recognizers whose backends are missing are reported and skipped —
the cascade degrades, it doesn't abort.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

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
        c.add_argument("--jersey", action="store_true", help="enable jersey-number OCR")
        c.add_argument("--faces", type=Path, help="roster image bank dir; enables face recognition")
        c.add_argument("--fps", type=float, default=0.5, help="frame sampling rate (default 0.5)")
        c.add_argument("--min-confidence", type=float, default=0.0)

    s = sub.add_parser("serve", help="run the recognition backend for dir2mcp")
    s.add_argument("--roster", type=Path, required=True)
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
    vision_flags(e)
    return p


def _pipeline(args, roster: Roster, games) -> Pipeline:
    return Pipeline(
        roster=roster,
        games=games,
        scorebug=args.scorebug,
        jersey=args.jersey,
        faces_bank=args.faces,
        fps=args.fps,
        min_confidence=args.min_confidence,
    )


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
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

    pipeline = _pipeline(args, roster, {args.media.name: game})
    annotations = pipeline.annotations_for(args.media)
    for msg in pipeline.skipped:
        print(f"skipped: {msg}", file=sys.stderr)

    events = game.events()
    card = score_mod.score(annotations, events, alignment, roster)
    text = report_mod.render(card, alignment, roster, title=args.media.name)
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
