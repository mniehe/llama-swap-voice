import textwrap

LATIN_TERMINATORS = frozenset(".!?…")
LATIN_CLOSERS = frozenset("\"'”’)]»")
CJK_TERMINATORS = frozenset("。！？")
CJK_CLOSERS = frozenset("”’」』)")


def segment(text: str, max_chars: int) -> list[str]:
    segments: list[str] = []
    current = ""

    for piece in _pieces(text, max_chars):
        candidate = f"{current} {piece}" if current else piece
        if len(candidate) <= max_chars:
            current = candidate
            continue

        segments.append(current)
        current = piece

    if current:
        segments.append(current)

    return segments


def _pieces(text: str, max_chars: int):
    for raw in _sentences(text):
        sentence = " ".join(raw.split())
        if not sentence:
            continue

        if len(sentence) <= max_chars:
            yield sentence
            continue

        yield from textwrap.wrap(sentence, max_chars, break_on_hyphens=False)


def _sentences(text: str):
    """Splits after terminator runs in one pass, so a long run of them costs linear time.

    Latin terminators end a sentence only before whitespace or the end, so "3.5"
    stays whole; CJK terminators are not followed by spaces. Closing quotes and
    brackets after a run stay with its sentence.
    """
    start = 0
    index = 0

    while index < len(text):
        char = text[index]
        if char in LATIN_TERMINATORS:
            index = _skip(text, _skip(text, index, LATIN_TERMINATORS), LATIN_CLOSERS)
            if index == len(text) or text[index].isspace():
                yield text[start:index]
                start = index
            continue

        if char in CJK_TERMINATORS:
            index = _skip(text, _skip(text, index, CJK_TERMINATORS), CJK_CLOSERS)
            yield text[start:index]
            start = index
            continue

        index += 1

    yield text[start:]


def _skip(text: str, index: int, chars: frozenset[str]) -> int:
    while index < len(text) and text[index] in chars:
        index += 1
    return index
