"""The feed-free evaluation path (#741 milestone 1).

`eval` binds the game into the pipeline, which enables the play-by-play
recognizer, AND uses the same feed for ground truth. With both on, the headline
is largely a measurement of wall-clock alignment: play-by-play emits one `pitch`
cue per feed event, the scorer reads `pitch` annotations, and the feed is scored
against itself. `--vision-only` keeps the feed as labels and out of the cascade.
"""

from dirstral_annotator.cli import build_parser


def parse(*extra):
    return build_parser().parse_args(
        ["eval", "game.mp4", "--roster", "r.json", "--game-pk", "745123",
         "--anchor", "1789265400.0=60.0", *extra]
    )


def test_vision_only_is_off_by_default():
    """A corpus that HAS a feed should use it; this flag is for the archives
    that do not, so it must be opt-in."""
    assert parse().vision_only is False


def test_vision_only_is_a_flag_on_eval():
    assert parse("--vision-only").vision_only is True


def test_the_feed_is_still_required_with_vision_only():
    """The feed supplies ground truth, which is what a label set is. Dropping it
    would leave nothing to score against, so `--vision-only` narrows what runs
    as a recognizer and never what the labels are."""
    parser = build_parser()
    args = parser.parse_args(
        ["eval", "game.mp4", "--roster", "r.json", "--feed", "gumbo.json",
         "--anchor", "1789265400.0=60.0", "--vision-only"]
    )
    assert args.vision_only is True
    assert str(args.feed) == "gumbo.json"
