import json

import pytest

from dirstral_annotator.roster import Roster


@pytest.fixture
def roster(tmp_path):
    path = tmp_path / "roster.json"
    path.write_text(json.dumps([
        {"id": "player:webb-logan", "name": "Logan Webb", "number": "62",
         "aliases": ["Webb", "L. Webb"], "mlbam_id": 657277},
        {"id": "player:doval-camilo", "name": "Camilo Doval", "number": "75",
         "mlbam_id": 666808},
    ]))
    return Roster.load(path)


def test_exact_name(roster):
    player, conf = roster.resolve_name("Logan Webb")
    assert player.id == "player:webb-logan" and conf == 1.0


def test_alias_and_normalization(roster):
    player, conf = roster.resolve_name("  L. WEBB ")
    assert player.id == "player:webb-logan" and conf == 1.0


def test_fuzzy_ocr_noise(roster):
    hit = roster.resolve_name("Logan Wehb")  # one OCR-confused letter
    assert hit is not None
    player, conf = hit
    assert player.id == "player:webb-logan"
    assert conf < 1.0


def test_unrelated_name_rejected(roster):
    assert roster.resolve_name("Clayton Kershaw") is None
    assert roster.resolve_name("") is None


def test_by_number_and_mlbam(roster):
    assert roster.by_number("75").id == "player:doval-camilo"
    assert roster.by_number("#62").id == "player:webb-logan"
    assert roster.by_number("99") is None
    assert roster.by_mlbam(657277).id == "player:webb-logan"
    assert roster.by_mlbam(1) is None
