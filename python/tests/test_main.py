import json
import subprocess
import sys
from pathlib import Path

import pytest

from conftest import VOICE, raw_config
from qwen_tts_server.__main__ import EXIT_CONFIG

SOURCE_ROOT = Path(__file__).resolve().parent.parent
TIMEOUT = 30


def start(config_path):
    return subprocess.run(
        [sys.executable, "-B", "-m", "qwen_tts_server", "--port", "0", "--config", str(config_path)],
        cwd=SOURCE_ROOT,
        capture_output=True,
        text=True,
        timeout=TIMEOUT,
    )


def test_instruct_with_null_transcript_fails_startup_naming_the_voice(tmp_path):
    config = tmp_path / "config.json"
    voices = {"stefan": dict(VOICE, transcript=None, instruct="Speak warmly.")}
    config.write_text(json.dumps(raw_config(voices=voices)))

    result = start(config)

    assert result.returncode == EXIT_CONFIG
    assert "voice stefan: instruct needs a transcript" in result.stderr
    assert "Traceback" not in result.stderr


def test_config_that_is_not_an_object_fails_startup(tmp_path):
    config = tmp_path / "config.json"
    config.write_text("[]")

    result = start(config)

    assert result.returncode == EXIT_CONFIG
    assert "config must be a JSON object" in result.stderr
    assert "Traceback" not in result.stderr


def config_text(**overrides):
    return json.dumps(raw_config(**overrides))


@pytest.mark.parametrize(
    "text, message",
    [
        (config_text().replace('"queue_wait_seconds": 5', '"queue_wait_seconds": 1e999'), "queue_wait_seconds must be a finite positive number, got inf"),
        (config_text(voices={"robbie": dict(VOICE, temperature=float("nan"))}), "voice robbie: temperature must be a finite number, got nan"),
        ("[" * 100_000 + "]" * 100_000, "config is nested too deeply"),
        (config_text(voices={"rob\nbie": dict(VOICE, transcript=None, instruct="Speak warmly.")}), "voice rob\\nbie: instruct needs a transcript"),
    ],
    ids=["infinite queue wait", "NaN temperature", "deep nesting", "newline in a voice name"],
)
def test_an_invalid_config_exits_2_with_one_line(tmp_path, text, message):
    config = tmp_path / "config.json"
    config.write_text(text)

    result = start(config)

    assert result.returncode == EXIT_CONFIG
    assert result.stderr.count("\n") == 1
    assert message in result.stderr
