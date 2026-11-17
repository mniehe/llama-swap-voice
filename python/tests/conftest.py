import threading

import pytest

from qwen_tts_server.config import parse

VOICE = {
    "clip": "/voices/robbie.wav",
    "transcript": "I sat me down.",
    "instruct": None,
    "temperature": 0.7,
    "top_k": 50,
    "top_p": 1.0,
    "repetition_penalty": 1.05,
}


def raw_config(**overrides) -> dict:
    raw = {
        "model_dir": "/model",
        "language": "English",
        "chunk_size": 12,
        "queue_size": 2,
        "queue_wait_seconds": 5,
        "max_input_chars": 100,
        "max_segment_chars": 40,
        "max_new_tokens": 60,
        "voices": {"robbie": dict(VOICE), "stefan": dict(VOICE, transcript=None)},
    }
    raw.update(overrides)
    return raw


@pytest.fixture
def make_config():
    return lambda **overrides: parse(raw_config(**overrides))


class FakeEngine:
    def __init__(self, chunks=(b"\x01\x00", b"\x02\x00"), error=None, fail_at=None):
        self.chunks = list(chunks)
        self.error = error
        self.fail_at = fail_at
        self.calls = []
        self.release_load = threading.Event()
        self.release_load.set()

    def load(self):
        self.release_load.wait()

    def stream(self, voice, segments):
        self.calls.append((voice, segments))
        for index, chunk in enumerate(self.chunks):
            if index == self.fail_at:
                raise self.error
            yield chunk


class SteppedEngine:
    """Produces one chunk per release, so a test controls exactly how far generation runs."""

    def __init__(self):
        self.gate = threading.Semaphore(0)
        self.produced = 0
        self.started = threading.Event()
        self.closed = threading.Event()

    def load(self):
        pass

    def stream(self, voice, segments):
        self.started.set()
        try:
            while True:
                self.gate.acquire()
                self.produced += 1
                yield bytes([self.produced])
        finally:
            self.closed.set()


@pytest.fixture
def fake_engine():
    return FakeEngine()
