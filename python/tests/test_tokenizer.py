import json
import os

from qwen_tts.core.tokenizer_12hz.modeling_qwen3_tts_tokenizer_v2 import Qwen3TTSTokenizerV2Encoder
from transformers import MimiConfig


def test_speech_tokenizer_encoder_constructs_from_pinned_config():
    """transformers 5.22 turned MimiModel.upsample into a read-only property, which breaks this constructor."""
    with open(os.environ["QWEN_TTS_TOKENIZER_CONFIG"], encoding="utf-8") as file:
        encoder_config = json.load(file)["encoder_config"]

    encoder = Qwen3TTSTokenizerV2Encoder(MimiConfig(**encoder_config))

    assert encoder.upsample is None
    assert encoder.decoder is None
