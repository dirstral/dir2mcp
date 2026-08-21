"""Roster loading and fuzzy name resolution.

The roster file is the pilot's source of canonical entity ids: recognizers
resolve whatever they saw (an OCR'd overlay name, a jersey number, a face
match) to a `Player`, so every cue speaks the same entity vocabulary and
retrieval-side text matching stays consistent (design 0004 §3 limitation 3).

Format (`roster.json`):
    [{"id": "player:webb-logan", "name": "Logan Webb", "number": "62",
      "aliases": ["Webb", "L. Webb"], "mlbam_id": 657277}]

`mlbam_id` is optional; when present it links the player to MLB statsapi /
Statcast identities so play-by-play ground truth maps onto roster ids.
"""

from __future__ import annotations

import difflib
import json
from pathlib import Path

from .model import Player

# Minimum similarity for a fuzzy name hit. OCR noise sits well above this
# for real overlay text; unrelated names sit well below.
MATCH_THRESHOLD = 0.72


def display_name(roster: "Roster | None", player_id: str) -> str:
    """The human-readable label for an entity id.

    Falls back to the id's own slug when the roster does not know the player,
    so a caller always has something printable. `emit.build_response` uses the
    same rule for the wire `entities` dictionary, and a recognizer uses it for
    cue text, so the two can never disagree about what a player is called.
    """
    player = roster.get(player_id) if roster else None
    if player:
        return player.name
    return player_id.split(":", 1)[-1].replace("-", " ").title()


class Roster:
    def __init__(self, players: list[Player], mlbam_ids: dict[int, str] | None = None):
        self.players = players
        self._by_id = {p.id: p for p in players}
        self._mlbam = mlbam_ids or {}
        # normalized alias -> player id (exact-match fast path)
        self._alias_index: dict[str, str] = {}
        for p in players:
            for name in p.all_names():
                self._alias_index[_norm(name)] = p.id

    @classmethod
    def load(cls, path: str | Path) -> "Roster":
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
        players, mlbam = [], {}
        for row in raw:
            p = Player(
                id=row["id"],
                name=row["name"],
                number=str(row["number"]) if row.get("number") is not None else None,
                aliases=tuple(row.get("aliases", ())),
            )
            players.append(p)
            if row.get("mlbam_id") is not None:
                mlbam[int(row["mlbam_id"])] = p.id
        return cls(players, mlbam)

    def get(self, player_id: str) -> Player | None:
        return self._by_id.get(player_id)

    def by_mlbam(self, mlbam_id: int) -> Player | None:
        pid = self._mlbam.get(mlbam_id)
        return self._by_id.get(pid) if pid else None

    def by_number(self, number: str) -> Player | None:
        number = number.lstrip("#").lstrip("0") or "0"
        for p in self.players:
            if p.number and (p.number.lstrip("0") or "0") == number:
                return p
        return None

    def resolve_name(self, text: str) -> tuple[Player, float] | None:
        """Resolve possibly-noisy text (OCR output) to a player.

        Exact normalized alias match wins at confidence 1.0; otherwise the
        best fuzzy alias match above MATCH_THRESHOLD, with the similarity
        ratio as confidence. Returns None when nothing clears the bar.
        """
        q = _norm(text)
        if not q:
            return None
        if q in self._alias_index:
            return self._by_id[self._alias_index[q]], 1.0
        best: tuple[Player, float] | None = None
        for alias, pid in self._alias_index.items():
            ratio = difflib.SequenceMatcher(None, q, alias).ratio()
            if ratio >= MATCH_THRESHOLD and (best is None or ratio > best[1]):
                best = (self._by_id[pid], ratio)
        return best


def _norm(s: str) -> str:
    return " ".join(s.lower().replace(".", " ").replace(",", " ").split())
