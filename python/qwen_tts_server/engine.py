import hashlib
import time
from contextlib import closing
from dataclasses import dataclass
from typing import Iterator

import numpy as np
import soundfile as sf

from . import wav
from .config import Config, Voice
from .events import emit

# The ICL prompt ends on the clip's last codec frame; trailing silence keeps its
# final phoneme from bleeding into the first generated word.
CLIP_TAIL_SILENCE_SECONDS = 0.5
SEGMENT_GAP_SECONDS = 0.15
WARM_TEXT = "Good morning. The kitchen lights are now off."


class EngineError(Exception):
    pass


class RunawaySegment(EngineError):
    pass


@dataclass(frozen=True)
class Prompt:
    voice: Voice
    vcp: dict
    prefill: int


class Engine:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.model = None
        self.prompts: dict[str, Prompt] = {}

    def load(self) -> None:
        # Imported here so this module, and the tests that fake the model, load without torch.
        from faster_qwen3_tts import FasterQwen3TTS

        started = time.monotonic()
        self.model = FasterQwen3TTS.from_pretrained(self.cfg.model_dir, local_files_only=True)
        if self.model.sample_rate != wav.SAMPLE_RATE:
            raise EngineError(f"model sample rate {self.model.sample_rate} is not {wav.SAMPLE_RATE}")

        self.model.warmup()
        emit("model_ready", seconds=_since(started))

        for name, voice in self.cfg.voices.items():
            self._add_voice(name, voice)

    def stream(self, name: str, segments: list[str]) -> Iterator[bytes]:
        prompt = self.prompts[name]
        for index, text in enumerate(segments):
            if index:
                yield wav.silence(SEGMENT_GAP_SECONDS)
            yield from self._generate(prompt, text)

    def _add_voice(self, name: str, voice: Voice) -> None:
        started = time.monotonic()
        vcp = self._voice_clone_prompt(voice)
        prefill = self._prefill(voice, vcp)

        # The talker's static cache stops silently at max_seq_len - 1.
        limit = self.model.max_seq_len - 1
        if prefill + self.cfg.max_new_tokens >= limit:
            raise EngineError(f"voice {name}: prefill {prefill} + max_new_tokens {self.cfg.max_new_tokens} reaches {limit}")

        self.prompts[name] = Prompt(voice, vcp, prefill)
        for _ in self.stream(name, [WARM_TEXT]):
            pass

        emit("voice_ready", voice=name, seconds=_since(started), prefill=prefill, clip_sha256=_sha256(voice.clip))

    def _voice_clone_prompt(self, voice: Voice) -> dict:
        audio, rate = _read_mono(voice.clip)
        base = self.model.model
        if voice.transcript:
            items = base.create_voice_clone_prompt(ref_audio=(_with_tail_silence(audio, rate), rate), ref_text=voice.transcript)
        else:
            items = base.create_voice_clone_prompt(ref_audio=(audio, rate), ref_text="", x_vector_only_mode=True)

        return base._prompt_items_to_voice_clone_prompt(items)

    def _prefill(self, voice: Voice, vcp: dict) -> int:
        # In streaming mode the prefill holds the reference frames and prefix only;
        # target text feeds in per step, so any text measures the same length.
        _, _, _, talker_input_embeds, *_ = self.model._prepare_generation(
            text=WARM_TEXT,
            language=self.cfg.language,
            ref_text=voice.transcript or "",
            voice_clone_prompt=vcp,
            instruct=voice.instruct,
        )
        return talker_input_embeds.shape[1]

    def _generate(self, prompt: Prompt, text: str) -> Iterator[bytes]:
        voice = prompt.voice
        chunks = self.model.generate_voice_clone_streaming(
            text=text,
            language=self.cfg.language,
            ref_text=voice.transcript or "",
            voice_clone_prompt=prompt.vcp,
            instruct=voice.instruct,
            max_new_tokens=self.cfg.max_new_tokens,
            chunk_size=self.cfg.chunk_size,
            temperature=voice.temperature,
            top_k=voice.top_k,
            top_p=voice.top_p,
            repetition_penalty=voice.repetition_penalty,
        )
        with closing(chunks):
            for audio, _rate, info in chunks:
                if info["total_steps_so_far"] >= self.cfg.max_new_tokens:
                    raise RunawaySegment(f"segment reached {self.cfg.max_new_tokens} steps without ending: {text[:40]!r}")
                yield wav.to_s16le(audio)


def _read_mono(path: str) -> tuple[np.ndarray, int]:
    audio, rate = sf.read(path, dtype="float32", always_2d=True)
    return audio.mean(axis=1), rate


def _with_tail_silence(audio: np.ndarray, rate: int) -> np.ndarray:
    tail = np.zeros(round(CLIP_TAIL_SILENCE_SECONDS * rate), dtype=np.float32)
    return np.concatenate([audio, tail])


def _sha256(path: str) -> str:
    with open(path, "rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def _since(started: float) -> float:
    return round(time.monotonic() - started, 2)
