"""Language of a short answer, for uk / ru / ky / ka / en.

Ported unchanged from /mnt/data/rfe-val/detect.py on q2e, the detector that
scored the #964 number (24 of 24). A character-only detector is not good
enough here: a one-sentence Ukrainian answer can contain no i/yi/ye, and a
Kyrgyz one contains the Cyrillic y that also marks Russian. So distinctive
letters and distinctive function words both vote, and a tie or a shortage of
evidence is reported as unknown rather than guessed.
"""
import re

LETTERS = {
    "uk": "їієґ",
    "ru": "ыэъё",
    "ky": "ңөү",
}
# Function words that exist in one of these languages and not in the others.
# Kept to closed-class words, which a one-sentence answer almost always uses.
WORDS = {
    "uk": ["що", "це", "який", "яка", "каже", "мова", "цьому", "також", "його",
           "має", "були", "було", "немає", "не", "як", "де", "коли", "про те"],
    "ru": ["что", "это", "который", "которая", "говорит", "также", "его",
           "есть", "были", "было", "нет", "как", "где", "когда", "о том"],
    "ky": ["жөнүндө", "деп", "айтат", "болгон", "эмне", "менен", "үчүн",
           "боюнча", "жана", "бул", "экенин", "айтылат", "тууралуу"],
}
GEORGIAN = re.compile(r"[Ⴀ-ჿ]")
CYRILLIC = re.compile(r"[Ѐ-ӿ]")
LATIN = re.compile(r"[A-Za-z]")

# A citation tag as dir2mcp renders it: [file@t=m:ss ...]. Same regex as
# measure.py on q2e.
TAG = re.compile(r"\[[^\[\]@]+@t=\d+:\d{2}")


def detect(text):
    """Return (lang, evidence). lang is uk|ru|ky|ka|en|unknown|empty."""
    t = " " + re.sub(r"[^\w\s]", " ", text.lower()) + " "
    if not t.strip():
        return "empty", "no text"
    if len(GEORGIAN.findall(text)) > 3:
        return "ka", "georgian script"
    cyr, lat = len(CYRILLIC.findall(text)), len(LATIN.findall(text))
    if cyr < 12 and lat > cyr:
        return "en", f"latin {lat} vs cyrillic {cyr}"
    score, why = {}, {}
    for lang in ("uk", "ru", "ky"):
        letters = sum(t.count(c) for c in LETTERS[lang])
        words = sum(len(re.findall(r"\s" + re.escape(w) + r"\s", t)) for w in WORDS[lang])
        # A distinctive letter is weaker evidence than a distinctive word: a
        # Kyrgyz answer uses the Russian y freely, and a Ukrainian one may use
        # none of its own letters in a single sentence.
        score[lang] = letters + 3 * words
        why[lang] = f"{lang}: {letters} letters + {words} words"
    order = sorted(score, key=lambda k: -score[k])
    top, second = order[0], order[1]
    ev = ", ".join(why[k] for k in order)
    if score[top] == 0:
        return "unknown", "cyrillic, no distinctive evidence: " + ev
    if score[top] == score[second]:
        return "unknown", "tie: " + ev
    return top, ev


def citation_tags(text):
    """Number of [file@t=m:ss] citation tags in an answer."""
    return len(TAG.findall(text or ""))
