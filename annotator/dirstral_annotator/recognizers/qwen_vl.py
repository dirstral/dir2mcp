"""Qwen3-VL backend for the scene-caption recognizer: a CaptionFn and a ProbeFn.

This is the backend that the #923 measurement was made WITH, ported from the
calibration harness rather than reimplemented, because the numbers the
recognizer ships (CLAIM_THRESHOLD = 0.99, the exact probe wordings) were
measured on Qwen/Qwen3-VL-4B-Instruct reading P(yes) off the first answer
token. A different model, a different prompt, or a different way of turning
logits into a probability is a different instrument, and the threshold would
have to be re-measured against the 400 labelled frames before it meant
anything. So the two things that decide the calibration are pinned here as
constants and the code says so.

HOW P(yes) IS READ, and why not from the generated text. A chat completion
answers "yes" or "no", which is one bit; the gate needs a scalar it can
threshold at 0.99. The probe therefore runs a single forward pass and reads
the next-token distribution: the probability mass on the yes-spellings
divided by the mass on yes- plus no-spellings. Several spellings are summed
because tokenizers split " yes", "Yes" and "YES" into different single tokens
and the model may put its mass on any of them; a spelling that is not a
single token is skipped rather than approximated. If neither side carries any
mass the backend is not answering the question asked, and the recognizer's
contract for that is unavailability, not a silent 0 or 1 (a 0 would suppress
every claim, a 1 would publish every claim, and both look like a decision).

CAPABILITY-DRIVEN, like faces.default_embedder: importing torch/transformers
is attempted at construction and a missing extra raises RecognizerUnavailable
with the pip line, so the package imports with no GPU present and the
recognizer degrades exactly as the other heavy backends do. A CUDA device that
is requested but not available is likewise unavailability, not a silent fall
back to CPU: at 1.5 s per frame on a GPU, a CPU run of a full game would take
days and look like a hang.
"""

from __future__ import annotations

import logging
import math
from collections.abc import Callable
from pathlib import Path

from .base import RecognizerUnavailable
from .caption import CaptionFn, ProbeFn

#: The model the #923 calibration was measured on. CLAIM_THRESHOLD and the
#: CLAIM_PROBES wordings in caption.py belong to this model; changing it
#: invalidates them until re-measured.
DEFAULT_MODEL = "Qwen/Qwen3-VL-4B-Instruct"

#: Pixel budget per frame, as measured. #860 found that halving it changed the
#: ANSWER rather than the price, so it is part of the instrument.
MAX_PIXELS = 1280 * 720

#: The caption prompt, verbatim from the calibration harness. The 0.94 / 0.63
#: precision split the gate exists to fix was measured on these words, and the
#: recognizer's scene classifier reads the vocabulary this prompt elicits.
CAPTION_PROMPT = (
    "Describe what this broadcast frame shows in one sentence. "
    "Say whether the camera is on the field, the crowd, the dugout, a "
    "replay or a graphic. Name any visible crowd reaction or celebration. "
    "Do not guess names or the score.\n"
    "Then on a second line write: confidence: <a number between 0 and 1 for "
    "how sure you are the sentence is correct>"
)

#: Longest caption the harness allowed. Also the instrument: a longer budget
#: lets the model append clauses the classifier never saw during calibration.
CAPTION_MAX_NEW_TOKENS = 80

#: The spellings whose next-token mass counts as "yes" and as "no". Only
#: spellings that tokenize to ONE token are used; the others are skipped.
YES_SPELLINGS = ("yes", "Yes", " yes", " Yes", "YES")
NO_SPELLINGS = ("no", "No", " no", " No", "NO")

#: The self-reported "confidence: 0.NN" line the caption prompt asks for. It
#: measured 0.514 AUC (chance) in #923 and is NOT the gate; it is parsed so
#: the CaptionFn contract's confidence slot carries what the model said rather
#: than a made-up constant, and so an operator reading the raw cue can see the
#: number the model claimed next to the claim it got wrong.
_SELF_CONF_PREFIX = "confidence:"


def _parse_caption(raw: str) -> tuple[str, float]:
    """Split the model's two-line answer into (caption, self_confidence).

    The confidence defaults to 0.5 when the model omits or garbles the line:
    a neutral value that neither clears nor fails any sane min_confidence
    floor, because the missing number is the model's failure, not evidence
    about the frame.
    """
    caption_lines: list[str] = []
    conf = 0.5
    for line in raw.splitlines():
        stripped = line.strip()
        low = stripped.lower()
        if low.startswith(_SELF_CONF_PREFIX):
            try:
                value = float(low[len(_SELF_CONF_PREFIX):].strip())
            except ValueError:
                continue
            if 0.0 <= value <= 1.0:
                conf = value
            continue
        if stripped:
            caption_lines.append(stripped)
    return " ".join(caption_lines).strip(), conf


def yes_probability(
    logprobs_of: Callable[[str], float | None],
) -> float | None:
    """P(yes) = mass(yes spellings) / (mass(yes) + mass(no)).

    `logprobs_of(spelling)` returns the log-probability of that spelling's
    single token at the answer position, or None when the spelling is not a
    single token. Factored out of the torch path so the arithmetic the
    threshold depends on is testable without a model: this is the exact
    computation the 400-frame calibration used.
    """
    def mass(spellings: tuple[str, ...]) -> float:
        total = 0.0
        for word in spellings:
            lp = logprobs_of(word)
            if lp is None:
                continue
            total += math.exp(lp)
        return total

    yes, no = mass(YES_SPELLINGS), mass(NO_SPELLINGS)
    if yes + no <= 0.0:
        return None
    return yes / (yes + no)


#: Oldest transformers release whose Auto* classes know model_type "qwen3_vl".
#: Below it AutoConfig raises a bare ValueError before any weight is read, which
#: is not RecognizerUnavailable and would escape the CLI's degrade path. The
#: `caption` extra in pyproject.toml carries the same floor; this check is the
#: runtime half, for a venv that was assembled without the extra.
MIN_TRANSFORMERS = (4, 57, 0)


def transformers_version() -> tuple[int, ...]:
    """The installed transformers version as a comparable tuple (mockable)."""
    import transformers  # type: ignore

    parts = []
    for piece in str(transformers.__version__).split(".")[:3]:
        digits = "".join(ch for ch in piece if ch.isdigit())
        parts.append(int(digits) if digits else 0)
    return tuple(parts)


def _require_transformers(installed: tuple[int, ...]) -> None:
    if installed < MIN_TRANSFORMERS:
        want = ".".join(str(n) for n in MIN_TRANSFORMERS)
        have = ".".join(str(n) for n in installed)
        raise RecognizerUnavailable(
            f"scene captioning needs transformers >= {want} for model type "
            f"qwen3_vl (installed {have}); pip install 'dirstral-annotator[caption]'"
        )


def load_backend(
    model_name: str = DEFAULT_MODEL,
    device: str = "cuda:0",
    max_pixels: int = MAX_PIXELS,
) -> tuple[CaptionFn, ProbeFn]:
    """Load the model once and return (captioner, prober) sharing it.

    Both callables are batch-shaped per the caption.py contracts. Loading is
    eager and happens here rather than on first call, so a missing extra or an
    unavailable device fails at construction where the operator can see it,
    exactly as faces.default_embedder does.
    """
    try:
        import torch  # type: ignore
        from PIL import Image  # type: ignore
        from transformers import AutoModelForImageTextToText, AutoProcessor  # type: ignore
    except ImportError as exc:
        raise RecognizerUnavailable(
            "scene captioning needs torch + transformers + Pillow "
            "(pip install 'dirstral-annotator[caption]')"
        ) from exc

    if device.startswith("cuda") and not torch.cuda.is_available():
        # Deliberately not a CPU fallback. torch reports WHY in its own
        # warning (typically a wheel built for a newer CUDA than the driver),
        # and a CPU run of a three-hour game would take days and look like a
        # hang rather than a misconfiguration.
        raise RecognizerUnavailable(
            f"scene captioning was asked for {device} but torch reports no CUDA "
            "device; check that the installed torch wheel matches the driver's "
            "CUDA version (or pass --caption-device cpu for a small test run)"
        )

    _require_transformers(transformers_version())

    log = logging.getLogger(__name__)
    log.info("loading %s on %s (max_pixels=%d)", model_name, device, max_pixels)
    try:
        processor = AutoProcessor.from_pretrained(model_name, max_pixels=max_pixels)
        model = AutoModelForImageTextToText.from_pretrained(
            model_name, dtype=torch.bfloat16, device_map=device, attn_implementation="sdpa",
        )
    except Exception as exc:  # noqa: BLE001 - the whole point is to map ANY load failure
        # An unrecognised model type (ValueError from AutoConfig), missing or
        # partial weights (OSError), a config/key mismatch (KeyError) -- none of
        # these are RecognizerUnavailable on their own, and the serve CLI only
        # degrades on that type. Left unmapped, `serve --caption` would crash at
        # startup instead of coming up without captioning and saying why.
        raise RecognizerUnavailable(
            f"scene captioning could not load {model_name} on {device}: "
            f"{type(exc).__name__}: {exc}"
        ) from exc
    model.eval()
    tokenizer = processor.tokenizer

    # Resolved once: which spellings are single tokens, and their ids.
    single_token_ids: dict[str, int] = {}
    for word in YES_SPELLINGS + NO_SPELLINGS:
        ids = tokenizer.encode(word, add_special_tokens=False)
        if len(ids) == 1:
            single_token_ids[word] = ids[0]
    if not any(w in single_token_ids for w in YES_SPELLINGS) or \
            not any(w in single_token_ids for w in NO_SPELLINGS):
        raise RecognizerUnavailable(
            f"{model_name}'s tokenizer has no single-token yes/no spellings; the "
            "probe cannot read P(yes) from one answer token on this model"
        )

    def _inputs(paths: list[Path], prompt: str):
        messages = [
            [{"role": "user", "content": [
                {"type": "image", "image": Image.open(p).convert("RGB")},
                {"type": "text", "text": prompt},
            ]}]
            for p in paths
        ]
        return processor.apply_chat_template(
            messages, tokenize=True, add_generation_prompt=True,
            return_dict=True, return_tensors="pt", padding=True,
        ).to(model.device)

    @torch.no_grad()
    def captioner(paths: list[Path]) -> list[tuple[str, float]]:
        if not paths:
            return []
        inputs = _inputs(paths, CAPTION_PROMPT)
        out = model.generate(
            **inputs, max_new_tokens=CAPTION_MAX_NEW_TOKENS, do_sample=False,
        )
        prompt_len = inputs["input_ids"].shape[1]
        texts = processor.batch_decode(out[:, prompt_len:], skip_special_tokens=True)
        return [_parse_caption(t) for t in texts]

    @torch.no_grad()
    def prober(paths: list[Path], question: str) -> list[float]:
        if not paths:
            return []
        inputs = _inputs(paths, question)
        # One forward pass, no generation: the answer is the next-token
        # distribution at the last position of each prompt.
        logits = model(**inputs).logits[:, -1, :].float()
        logprobs = torch.log_softmax(logits, dim=-1)
        scores: list[float] = []
        for row in logprobs:
            def lp_of(word: str, row=row) -> float | None:
                tid = single_token_ids.get(word)
                return None if tid is None else float(row[tid].item())
            p = yes_probability(lp_of)
            if p is None:
                raise RecognizerUnavailable(
                    "probe returned no mass on any yes/no token; the model is not "
                    "answering the question asked"
                )
            scores.append(p)
        return scores

    return captioner, prober
