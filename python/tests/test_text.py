import random
import re
import textwrap
import time

import pytest

from qwen_tts_server.text import segment

MAX = 400


def test_short_text_is_one_segment():
    assert segment("The kitchen lights are now off.", MAX) == ["The kitchen lights are now off."]


def test_splits_on_unicode_terminators_and_closing_quotes():
    text = "He said “stop!” Then he left… Was it late？我们明天见面吧。Bye now."

    # 16 fits every sentence alone but no two together, so each lands in its own segment.
    assert segment(text, 16) == ["He said “stop!”", "Then he left…", "Was it late？", "我们明天见面吧。", "Bye now."]


def test_decimal_points_do_not_split():
    assert segment("It is 3.5 degrees. Wrap up.", 20) == ["It is 3.5 degrees.", "Wrap up."]


def test_packs_sentences_up_to_the_limit():
    sentence = "a" * 98 + "."
    text = " ".join([sentence] * 9)

    segments = segment(text, MAX)

    assert [len(s) for s in segments] == [399, 399, 99]
    assert all(len(s) <= MAX for s in segments)
    assert " ".join(segments) == text


def test_over_long_sentence_splits_at_word_boundaries():
    words = ["word"] * 200
    sentence = " ".join(words) + "."

    segments = segment(sentence, MAX)

    assert len(segments) == 3
    assert all(len(s) <= MAX for s in segments)
    assert " ".join(segments) == sentence


def test_over_long_word_is_cut():
    segments = segment("x" * 900, MAX)

    assert [len(s) for s in segments] == [400, 400, 100]


def test_whitespace_is_collapsed():
    assert segment("Line one.\n\nLine   two.", MAX) == ["Line one. Line two."]


def test_empty_text_has_no_segments():
    assert segment("   ", MAX) == []


# The sentence pattern segment() used before it became a linear scan; kept as
# the oracle the scan must agree with.
_REFERENCE = re.compile(
    r".*?(?:[.!?…]+[\"'”’)\]»]*(?=\s|$)|[。！？]+[”’」』)]*|$)",
    re.DOTALL,
)


def reference_segment(text: str, max_chars: int) -> list[str]:
    segments: list[str] = []
    current = ""
    for match in _REFERENCE.finditer(text):
        sentence = " ".join(match.group().split())
        if not sentence:
            continue
        pieces = [sentence] if len(sentence) <= max_chars else textwrap.wrap(sentence, max_chars, break_on_hyphens=False)
        for piece in pieces:
            candidate = f"{current} {piece}" if current else piece
            if len(candidate) <= max_chars:
                current = candidate
                continue
            segments.append(current)
            current = piece
    if current:
        segments.append(current)
    return segments


@pytest.mark.parametrize("terminator", [".", "!", "。"])
def test_a_long_run_of_terminators_is_split_in_linear_time(terminator):
    text = terminator * 100_000 + "x"

    started = time.monotonic()
    segments = segment(text, 400)

    assert time.monotonic() - started < 1
    assert "".join(segments) == text


@pytest.mark.parametrize(
    "text",
    [
        "Wait...no. Go!?Now ok.",
        "He said “stop!”x then left.",
        "3.5.x and 4!!) y. End",
        "End…”) next. 好的。。」再见！x",
        "Trailing dots...",
        "Line one.\nLine two!\n",
        "a.b.c. d",
        "",
        "  ",
    ],
)
def test_runs_of_terminators_without_a_following_space_split_as_before(text):
    assert segment(text, 12) == reference_segment(text, 12)
    assert segment(text, MAX) == reference_segment(text, MAX)


def test_matches_the_reference_on_random_text():
    alphabet = "ab .!?…\"'”’)]»。！？」』\n"
    rng = random.Random(1)

    for _ in range(3000):
        text = "".join(rng.choice(alphabet) for _ in range(rng.randint(0, 40)))
        assert segment(text, 8) == reference_segment(text, 8), repr(text)
