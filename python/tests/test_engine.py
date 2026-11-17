import sys
from dataclasses import replace
from types import ModuleType, SimpleNamespace

import numpy as np
import pytest
import soundfile as sf

from conftest import VOICE
from qwen_tts_server import wav
from qwen_tts_server.engine import CLIP_TAIL_SILENCE_SECONDS, SEGMENT_GAP_SECONDS, Engine, EngineError, Prompt, RunawaySegment

MAX_SEQ_LEN = 2048


class FakeModel:
    """Stands in for FasterQwen3TTS.generate_voice_clone_streaming with a fixed step count."""

    def __init__(self, steps):
        self.steps = steps
        self.calls = []
        self.closed = False

    def generate_voice_clone_streaming(self, text, max_new_tokens, chunk_size, **kwargs):
        self.calls.append(dict(kwargs, text=text, max_new_tokens=max_new_tokens))
        try:
            for total in range(chunk_size, self.steps + chunk_size, chunk_size):
                info = {"total_steps_so_far": min(total, self.steps, max_new_tokens)}
                yield np.full(4, 0.5, dtype=np.float32), wav.SAMPLE_RATE, info
                if info["total_steps_so_far"] >= max_new_tokens:
                    return
        finally:
            self.closed = True


class PrefillModel(FakeModel):
    """Adds the static-cache size and a prepared prompt of a chosen prefill length."""

    max_seq_len = MAX_SEQ_LEN

    def __init__(self, prefill):
        super().__init__(steps=12)
        self.prefill = prefill

    def _prepare_generation(self, **kwargs):
        return None, None, None, np.zeros((1, self.prefill)), None, None, None, None


def engine_with(model, make_config):
    engine = Engine(make_config())
    engine.model = model
    voice = engine.cfg.voices["robbie"]
    engine.prompts["robbie"] = Prompt(voice, {"icl_mode": [True]}, prefill=10)
    return engine


def test_stream_converts_chunks_and_passes_voice_settings(make_config):
    model = FakeModel(steps=24)
    engine = engine_with(model, make_config)

    chunks = list(engine.stream("robbie", ["Hello there."]))

    assert chunks == [wav.to_s16le(np.full(4, 0.5, dtype=np.float32))] * 2
    call = model.calls[0]
    assert call["text"] == "Hello there."
    assert call["ref_text"] == "I sat me down."
    assert call["voice_clone_prompt"] == {"icl_mode": [True]}
    assert call["temperature"] == 0.7
    assert call["max_new_tokens"] == 60


def test_segments_are_joined_with_silence(make_config):
    engine = engine_with(FakeModel(steps=12), make_config)

    chunks = list(engine.stream("robbie", ["One.", "Two."]))

    assert chunks[1] == wav.silence(SEGMENT_GAP_SECONDS)
    assert len(chunks) == 3


def test_runaway_segment_fails_the_job(make_config):
    model = FakeModel(steps=10_000)
    engine = engine_with(model, make_config)

    with pytest.raises(RunawaySegment, match="60 steps"):
        list(engine.stream("robbie", ["Never ends."]))

    assert model.closed


def add_voice(prefill, make_config, tmp_path):
    clip = tmp_path / "robbie.wav"
    clip.write_bytes(b"clip")
    engine = Engine(make_config())
    engine.model = PrefillModel(prefill)
    engine._voice_clone_prompt = lambda voice: {"icl_mode": [True]}
    engine._add_voice("robbie", replace(engine.cfg.voices["robbie"], clip=str(clip)))
    return engine


def test_voice_whose_prefill_would_fill_the_static_cache_is_rejected(make_config, tmp_path):
    limit = MAX_SEQ_LEN - 1
    prefill = limit - make_config().max_new_tokens

    with pytest.raises(EngineError, match=f"prefill {prefill}"):
        add_voice(prefill, make_config, tmp_path)


def test_voice_below_the_static_cache_limit_is_added(make_config, tmp_path):
    limit = MAX_SEQ_LEN - 1
    prefill = limit - make_config().max_new_tokens - 1

    engine = add_voice(prefill, make_config, tmp_path)

    assert engine.prompts["robbie"].prefill == prefill


class PromptModel:
    """Records how the voice clone prompt is requested from the base model."""

    def __init__(self):
        self.requests = []

    def create_voice_clone_prompt(self, ref_audio, ref_text, **kwargs):
        audio, rate = ref_audio
        self.requests.append(dict(kwargs, samples=len(audio), rate=rate, ref_text=ref_text))
        return ["items"]

    def _prompt_items_to_voice_clone_prompt(self, items):
        return {"items": items}


def prompt_request(voice, make_config, tmp_path):
    clip = tmp_path / "clip.wav"
    sf.write(clip, np.zeros(wav.SAMPLE_RATE, dtype=np.float32), wav.SAMPLE_RATE, subtype="PCM_16")
    engine = Engine(make_config())
    base = PromptModel()
    engine.model = SimpleNamespace(model=base)

    vcp = engine._voice_clone_prompt(replace(voice, clip=str(clip)))

    assert vcp == {"items": ["items"]}
    return base.requests[0]


def test_null_transcript_builds_an_embedding_only_prompt(make_config, tmp_path):
    voice = make_config().voices["stefan"]

    request = prompt_request(voice, make_config, tmp_path)

    assert request == {"x_vector_only_mode": True, "ref_text": "", "samples": wav.SAMPLE_RATE, "rate": wav.SAMPLE_RATE}


def test_transcript_builds_an_icl_prompt_with_tail_silence(make_config, tmp_path):
    voice = make_config().voices["robbie"]

    request = prompt_request(voice, make_config, tmp_path)

    tail = round(CLIP_TAIL_SILENCE_SECONDS * wav.SAMPLE_RATE)
    assert request == {"ref_text": "I sat me down.", "samples": wav.SAMPLE_RATE + tail, "rate": wav.SAMPLE_RATE}


class FakeTTS(PrefillModel):
    """Stands in for a loaded FasterQwen3TTS: base model, sample rate, warm-up and streaming."""

    def __init__(self, sample_rate):
        super().__init__(prefill=10)
        self.sample_rate = sample_rate
        self.model = PromptModel()
        self.warmed = False

    def warmup(self):
        self.warmed = True


def loading_engine(sample_rate, make_config, tmp_path, monkeypatch):
    model = FakeTTS(sample_rate)
    module = ModuleType("faster_qwen3_tts")
    module.FasterQwen3TTS = SimpleNamespace(from_pretrained=lambda path, local_files_only: model)
    monkeypatch.setitem(sys.modules, "faster_qwen3_tts", module)

    voices = {}
    for name, transcript in (("robbie", "I sat me down."), ("stefan", None)):
        clip = tmp_path / f"{name}.wav"
        sf.write(clip, np.zeros(wav.SAMPLE_RATE, dtype=np.float32), wav.SAMPLE_RATE, subtype="PCM_16")
        voices[name] = dict(VOICE, clip=str(clip), transcript=transcript)
    return Engine(make_config(voices=voices)), model


def test_load_refuses_a_model_at_another_sample_rate(make_config, tmp_path, monkeypatch):
    engine, model = loading_engine(22050, make_config, tmp_path, monkeypatch)

    with pytest.raises(EngineError, match="model sample rate 22050 is not 24000"):
        engine.load()

    assert not model.warmed
    assert engine.prompts == {}


def test_load_warms_up_and_builds_every_voice_prompt(make_config, tmp_path, monkeypatch):
    engine, model = loading_engine(wav.SAMPLE_RATE, make_config, tmp_path, monkeypatch)

    engine.load()

    assert model.warmed
    assert set(engine.prompts) == {"robbie", "stefan"}
    assert [request["ref_text"] for request in model.model.requests] == ["I sat me down.", ""]
