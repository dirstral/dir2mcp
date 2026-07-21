"""End-to-end: play-by-play labels from a saved GUMBO feed -> fused
annotations -> both sidecars on disk -> eval scores 100% against the same
ground truth. No network, no ffmpeg, no CV backends."""

import json
from pathlib import Path

from dirstral_annotator.cli import main
from dirstral_annotator.eval import ground_truth

FIXTURE = Path(__file__).parent / "fixtures" / "gumbo_min.json"


def setup_corpus(tmp_path):
    media = tmp_path / "game7.mp4"
    media.write_bytes(b"\x00")
    roster = tmp_path / "roster.json"
    roster.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": 657277},
    ]))
    events = ground_truth.parse_pitches(ground_truth.load_game(FIXTURE))
    # anchor: first pitch appears at video 60.0s
    anchor = f"{events[0].epoch_s}=60.0"
    return media, roster, anchor


def test_annotate_writes_both_sidecars(tmp_path, capsys):
    media, roster, anchor = setup_corpus(tmp_path)
    rc = main(["annotate", str(media), "--roster", str(roster),
               "--feed", str(FIXTURE), "--anchor", anchor])
    assert rc == 0

    vtt = (tmp_path / "game7.vtt").read_text()
    assert vtt.startswith("WEBVTT")
    assert "Pitch: Logan Webb to Freddie Freeman" in vtt
    assert "confidence 0.98" in vtt
    # first pitch anchored at 60s, pre-roll 3s
    assert "00:00:57.000 -->" in vtt

    payload = json.loads((tmp_path / "game7.annotations.json").read_text())
    assert payload["media"] == "game7.mp4"
    assert len(payload["annotations"]) == 4
    assert all(a["event"] == "pitch" for a in payload["annotations"])
    assert payload["entities"][0]["id"] == "player:webb-logan"


def test_eval_scores_green_against_own_ground_truth(tmp_path, capsys):
    media, roster, anchor = setup_corpus(tmp_path)
    report = tmp_path / "report.md"
    rc = main(["eval", str(media), "--roster", str(roster),
               "--feed", str(FIXTURE), "--anchor", anchor,
               "--report", str(report)])
    assert rc == 0  # recall/precision over pilot targets
    text = report.read_text()
    assert "recall: **100.0%**" in text
    assert "Logan Webb" in text


def test_playbyplay_requires_anchor(tmp_path):
    media, roster, _ = setup_corpus(tmp_path)
    try:
        main(["annotate", str(media), "--roster", str(roster), "--feed", str(FIXTURE)])
    except SystemExit as exc:
        assert "--anchor" in str(exc)
    else:
        raise AssertionError("expected SystemExit")
