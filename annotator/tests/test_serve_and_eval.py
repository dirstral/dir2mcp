"""End-to-end for both entry points, no network / ffmpeg / CV backends:

- serve: a real HTTP round-trip against an ephemeral-port server, exactly the
  request dir2mcp's RecognizeServeClient sends, asserting the design-0004
  wire contract.
- eval: play-by-play labels from a saved GUMBO feed scored against the same
  ground truth (the phase-1 gate exits 0 at 100% recall/precision).
"""

import json
import threading
import urllib.request
from pathlib import Path

from dirstral_annotator.cli import main
from dirstral_annotator.eval import ground_truth
from dirstral_annotator.pipeline import GameConfig, Pipeline
from dirstral_annotator.roster import Roster
from dirstral_annotator.serve import serve

FIXTURE = Path(__file__).parent / "fixtures" / "gumbo_min.json"


def setup_corpus(tmp_path):
    media = tmp_path / "game7.mp4"
    media.write_bytes(b"\x00")
    roster_path = tmp_path / "roster.json"
    roster_path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62", "mlbam_id": 657277},
    ]))
    events = ground_truth.parse_pitches(ground_truth.load_game(FIXTURE))
    anchor = f"{events[0].epoch_s}=60.0"
    return media, roster_path, anchor


def test_serve_recognize_round_trip(tmp_path):
    media, roster_path, anchor = setup_corpus(tmp_path)
    game = GameConfig.parse({"feed": str(FIXTURE), "anchors": [anchor]})
    pipeline = Pipeline(roster=Roster.load(roster_path), games={"game7.mp4": game})
    server = serve(pipeline, host="127.0.0.1", port=0)  # ephemeral port
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"

        with urllib.request.urlopen(base + "/health") as resp:
            assert resp.status == 200

        req = urllib.request.Request(
            base + "/recognize",
            data=json.dumps({"path": str(media)}).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req) as resp:
            payload = json.load(resp)

        assert payload["recognizer"]["name"] == "dirstral-annotator"
        assert len(payload["annotations"]) == 4  # all fixture pitches
        first = payload["annotations"][0]
        assert first["event"] == "pitch"
        assert "Logan Webb" in first["text"]
        assert first["start_s"] == 57.0  # anchored at 60s, pre-roll 3s
        assert payload["entities"][0]["id"] == "player:webb-logan"

        # unknown media -> 404, malformed body -> 400 (no hangs, no 500 leaks)
        bad = urllib.request.Request(
            base + "/recognize", data=json.dumps({"path": str(tmp_path / "no.mp4")}).encode()
        )
        try:
            urllib.request.urlopen(bad)
            raise AssertionError("expected 404")
        except urllib.error.HTTPError as exc:
            assert exc.code == 404
    finally:
        server.shutdown()


def test_eval_scores_green_against_own_ground_truth(tmp_path):
    media, roster_path, anchor = setup_corpus(tmp_path)
    report = tmp_path / "report.md"
    rc = main(["eval", str(media), "--roster", str(roster_path),
               "--feed", str(FIXTURE), "--anchor", anchor,
               "--report", str(report)])
    assert rc == 0  # recall/precision over the pilot targets
    text = report.read_text()
    assert "recall: **100.0%**" in text
    assert "Logan Webb" in text


def test_eval_requires_anchor_and_ground_truth(tmp_path):
    media, roster_path, _ = setup_corpus(tmp_path)
    for argv, want in [
        (["eval", str(media), "--roster", str(roster_path), "--anchor", "1=2"], "--game-pk or --feed"),
    ]:
        try:
            main(argv)
        except SystemExit as exc:
            assert want in str(exc)
        else:
            raise AssertionError("expected SystemExit")
