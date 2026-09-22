"""Text normalisation shared by every metric.

The two decoders under comparison differ in surface form: whisper emits casing
and punctuation, MMS (CTC) emits lowercase with none. Agreement must not pay
for that, so both sides go through the same normaliser before tokenisation.
Digits are kept as tokens: whisper writes "2005" where MMS spells the numeral
out, and that is a real disagreement the rig reports rather than hides.
"""
import re
import unicodedata

_NON_WORD = re.compile(r"[^\w\s]", re.UNICODE)
_SPACE = re.compile(r"\s+")
# Apostrophe-like marks inside a word (Ukrainian "п'ять", "м’який") are part of
# the word for a speaker; dropping them rather than splitting keeps one token.
_APOSTROPHE = re.compile(r"[’ʼ'`]")


def normalize(text):
    """Lowercase, NFKC, no punctuation, single spaces, ё folded to е."""
    if not text:
        return ""
    t = unicodedata.normalize("NFKC", text).lower()
    t = _APOSTROPHE.sub("", t)
    t = t.replace("ё", "е")
    t = _NON_WORD.sub(" ", t)
    t = t.replace("_", " ")
    return _SPACE.sub(" ", t).strip()


def tokens(text):
    """Normalised whitespace tokens."""
    n = normalize(text)
    return n.split() if n else []
