import numpy as np

from qwen_tts_server import wav

EXPECTED_HEADER = bytes.fromhex(
    "52494646"  # RIFF
    "ffffffff"  # unknown RIFF size
    "57415645"  # WAVE
    "666d7420"  # "fmt "
    "10000000"  # fmt chunk size 16
    "0100"  # PCM
    "0100"  # mono
    "c05d0000"  # 24000 Hz
    "80bb0000"  # 48000 bytes/s
    "0200"  # block align 2
    "1000"  # 16 bits
    "64617461"  # data
    "ffffffff"  # unknown data size
)


def test_header_bytes():
    assert wav.header() == EXPECTED_HEADER
    assert len(wav.header()) == 44


def test_s16le_conversion_scales_and_clips():
    samples = np.array([0.0, 1.0, -1.0, 0.5, 2.0, -2.0], dtype=np.float32)

    data = wav.to_s16le(samples)

    assert np.frombuffer(data, dtype="<i2").tolist() == [0, 32767, -32767, 16383, 32767, -32767]


def test_s16le_is_little_endian():
    assert wav.to_s16le(np.array([1.0], dtype=np.float32)) == b"\xff\x7f"


def test_silence_length():
    assert wav.silence(0.15) == bytes(3600 * 2)
