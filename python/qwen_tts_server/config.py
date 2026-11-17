import json
import math
from dataclasses import dataclass, fields


class ConfigError(ValueError):
    pass


@dataclass(frozen=True)
class Voice:
    clip: str
    transcript: str | None
    instruct: str | None
    temperature: float
    top_k: int
    top_p: float
    repetition_penalty: float


@dataclass(frozen=True)
class Config:
    model_dir: str
    language: str
    chunk_size: int
    queue_size: int
    queue_wait_seconds: float
    max_input_chars: int
    max_segment_chars: int
    max_new_tokens: int
    voices: dict[str, Voice]


POSITIVE_INTEGER_SETTINGS = ("chunk_size", "queue_size", "max_input_chars", "max_segment_chars", "max_new_tokens")
POSITIVE_NUMBER_SETTINGS = ("queue_wait_seconds",)
STRING_SETTINGS = ("model_dir", "language")
VOICE_NUMBERS = ("temperature", "top_p", "repetition_penalty")
VOICE_OPTIONAL_STRINGS = ("transcript", "instruct")


def load(path: str) -> Config:
    with open(path, encoding="utf-8") as file:
        try:
            raw = json.load(file)
        except RecursionError:
            raise ConfigError("config is nested too deeply to parse") from None
    return parse(raw)


def parse(raw) -> Config:
    if not isinstance(raw, dict):
        raise ConfigError("config must be a JSON object")

    raw_voices = raw.get("voices", {})
    if not isinstance(raw_voices, dict):
        raise ConfigError("voices must be an object")

    voices = {name: _voice(name, value) for name, value in raw_voices.items()}
    if not voices:
        raise ConfigError("config has no voices")

    settings = {}
    for field in fields(Config):
        if field.name == "voices":
            continue
        if field.name not in raw:
            raise ConfigError(f"missing setting {field.name}")
        settings[field.name] = raw[field.name]

    _check_settings(settings)
    return Config(**settings, voices=voices)


def _check_settings(settings: dict) -> None:
    for key in POSITIVE_INTEGER_SETTINGS:
        value = settings[key]
        if not (_is_integer(value) and value > 0):
            raise ConfigError(f"{key} must be a positive integer, got {value!r}")

    for key in POSITIVE_NUMBER_SETTINGS:
        value = settings[key]
        if not (_is_number(value) and value > 0):
            raise ConfigError(f"{key} must be a finite positive number, got {value!r}")

    for key in STRING_SETTINGS:
        value = settings[key]
        if not isinstance(value, str):
            raise ConfigError(f"{key} must be a string, got {value!r}")


def _voice(name: str, raw) -> Voice:
    shown = _escaped(name)
    if not isinstance(raw, dict):
        raise ConfigError(f"voice {shown} must be an object")

    keys = [field.name for field in fields(Voice)]
    unknown = sorted(set(raw) - set(keys))
    missing = [key for key in keys if key not in raw]
    if unknown:
        raise ConfigError(f"voice {shown}: unknown key {unknown[0]!r}")
    if missing:
        raise ConfigError(f"voice {shown}: missing key {missing[0]!r}")

    voice = Voice(**raw)
    problem = _voice_problem(voice)
    if problem:
        raise ConfigError(f"voice {shown}: {problem}")

    return voice


def _voice_problem(voice: Voice) -> str | None:
    if not isinstance(voice.clip, str):
        return f"clip must be a string, got {voice.clip!r}"

    for key in VOICE_OPTIONAL_STRINGS:
        value = getattr(voice, key)
        if value is not None and not isinstance(value, str):
            return f"{key} must be a string or null, got {value!r}"

    for key in VOICE_NUMBERS:
        value = getattr(voice, key)
        if not _is_number(value):
            return f"{key} must be a finite number, got {value!r}"

    if not _is_integer(voice.top_k):
        return f"top_k must be an integer, got {voice.top_k!r}"

    # null selects embedding-only cloning; an empty string is a missing transcript.
    if voice.transcript is not None and not voice.transcript.strip():
        return "transcript is empty; use null for embedding-only cloning"
    if voice.instruct and voice.transcript is None:
        return "instruct needs a transcript"

    return None


# bool is an int subclass, so true would otherwise pass as 1.
def _is_integer(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


# json.load reads 1e999 as inf and accepts NaN, neither of which is a usable setting.
def _is_number(value) -> bool:
    if isinstance(value, float):
        return math.isfinite(value)
    return _is_integer(value)


def _escaped(name: str) -> str:
    """The name with control characters escaped as repr() would, so a diagnostic stays on one line."""
    return repr(name)[1:-1]
