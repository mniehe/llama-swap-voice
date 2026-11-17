import pytest

from conftest import VOICE, raw_config
from qwen_tts_server.config import ConfigError, parse


def test_parses_voices_and_settings():
    cfg = parse(raw_config())

    assert cfg.max_new_tokens == 60
    assert cfg.voices["robbie"].transcript == "I sat me down."
    assert cfg.voices["stefan"].transcript is None


def test_rejects_instruct_without_transcript():
    voices = {"stefan": dict(VOICE, transcript=None, instruct="Speak warmly.")}

    with pytest.raises(ConfigError, match="voice stefan: instruct needs a transcript"):
        parse(raw_config(voices=voices))


@pytest.mark.parametrize("transcript", ["", "   "])
def test_rejects_empty_transcript(transcript):
    voices = {"robbie": dict(VOICE, transcript=transcript)}

    with pytest.raises(ConfigError, match="voice robbie: transcript is empty"):
        parse(raw_config(voices=voices))


def test_accepts_instruct_with_transcript():
    voices = {"robbie": dict(VOICE, instruct="Speak warmly.")}

    assert parse(raw_config(voices=voices)).voices["robbie"].instruct == "Speak warmly."


def test_rejects_empty_voices():
    with pytest.raises(ConfigError, match="no voices"):
        parse(raw_config(voices={}))


def test_rejects_unknown_voice_key():
    voices = {"robbie": dict(VOICE, pitch=2)}

    with pytest.raises(ConfigError, match="voice robbie"):
        parse(raw_config(voices=voices))


def test_rejects_missing_setting():
    raw = raw_config()
    del raw["max_new_tokens"]

    with pytest.raises(ConfigError, match="^missing setting max_new_tokens"):
        parse(raw)


POSITIVE_INTEGERS = ["chunk_size", "queue_size", "max_input_chars", "max_segment_chars", "max_new_tokens"]


@pytest.mark.parametrize("key", POSITIVE_INTEGERS)
@pytest.mark.parametrize("value", [0, -1, 1.5, "12", True, None])
def test_rejects_a_setting_that_is_not_a_positive_integer(key, value):
    with pytest.raises(ConfigError, match=f"^{key} must be a positive integer"):
        parse(raw_config(**{key: value}))


@pytest.mark.parametrize("value", [0, -0.5, "5", True, None, float("inf"), float("nan")])
def test_rejects_a_queue_wait_that_is_not_a_positive_number(value):
    with pytest.raises(ConfigError, match="^queue_wait_seconds must be a finite positive number"):
        parse(raw_config(queue_wait_seconds=value))


def test_accepts_a_fractional_queue_wait():
    assert parse(raw_config(queue_wait_seconds=0.5)).queue_wait_seconds == 0.5


@pytest.mark.parametrize("key", ["model_dir", "language"])
@pytest.mark.parametrize("value", [7, None, ["x"]])
def test_rejects_a_setting_that_is_not_a_string(key, value):
    with pytest.raises(ConfigError, match=f"^{key} must be a string"):
        parse(raw_config(**{key: value}))


@pytest.mark.parametrize(
    "key, value, message",
    [
        ("clip", 7, "clip must be a string"),
        ("clip", None, "clip must be a string"),
        ("transcript", 7, "transcript must be a string or null"),
        ("transcript", ["I sat"], "transcript must be a string or null"),
        ("instruct", 7, "instruct must be a string or null"),
        ("temperature", "warm", "temperature must be a finite number"),
        ("temperature", True, "temperature must be a finite number"),
        ("top_k", 1.5, "top_k must be an integer"),
        ("top_k", True, "top_k must be an integer"),
        ("top_p", None, "top_p must be a finite number"),
        ("repetition_penalty", "1.05", "repetition_penalty must be a finite number"),
        ("temperature", float("nan"), "temperature must be a finite number"),
        ("top_p", float("inf"), "top_p must be a finite number"),
    ],
)
def test_rejects_a_voice_field_of_the_wrong_type(key, value, message):
    voices = {"robbie": dict(VOICE, **{key: value})}

    with pytest.raises(ConfigError, match=f"^voice robbie: {message}"):
        parse(raw_config(voices=voices))


@pytest.mark.parametrize("raw", [[], "config", 7, None])
def test_rejects_a_config_that_is_not_an_object(raw):
    with pytest.raises(ConfigError, match="^config must be a JSON object"):
        parse(raw)


def test_rejects_voices_that_are_not_an_object():
    with pytest.raises(ConfigError, match="^voices must be an object"):
        parse(raw_config(voices=[dict(VOICE)]))


def test_rejects_a_voice_that_is_not_an_object():
    with pytest.raises(ConfigError, match="^voice robbie must be an object"):
        parse(raw_config(voices={"robbie": "robbie.wav"}))


def test_a_voice_name_is_escaped_in_messages():
    voices = {"rob\nbie": dict(VOICE, transcript="")}

    with pytest.raises(ConfigError, match=r"^voice rob\\nbie: transcript is empty"):
        parse(raw_config(voices=voices))


def test_an_unknown_voice_key_is_named():
    voices = {"robbie": dict(VOICE, **{"pi\ntch": 2})}

    with pytest.raises(ConfigError, match=r"^voice robbie: unknown key 'pi\\ntch'$"):
        parse(raw_config(voices=voices))


def test_a_missing_voice_key_is_named():
    voice = dict(VOICE)
    del voice["top_k"]

    with pytest.raises(ConfigError, match="^voice robbie: missing key 'top_k'$"):
        parse(raw_config(voices={"robbie": voice}))
