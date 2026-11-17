import struct

import numpy as np

SAMPLE_RATE = 24000
CHANNELS = 1
SAMPLE_BYTES = 2
BITS_PER_SAMPLE = SAMPLE_BYTES * 8
PCM_FORMAT = 1
FMT_CHUNK_BYTES = 16
# Streaming length is unknown up front; readers treat 0xFFFFFFFF as "until EOF".
UNKNOWN_SIZE = 0xFFFFFFFF
INT16_MAX = 32767


def header() -> bytes:
    return struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        UNKNOWN_SIZE,
        b"WAVE",
        b"fmt ",
        FMT_CHUNK_BYTES,
        PCM_FORMAT,
        CHANNELS,
        SAMPLE_RATE,
        SAMPLE_RATE * CHANNELS * SAMPLE_BYTES,
        CHANNELS * SAMPLE_BYTES,
        BITS_PER_SAMPLE,
        b"data",
        UNKNOWN_SIZE,
    )


def to_s16le(samples: np.ndarray) -> bytes:
    clipped = np.clip(np.asarray(samples, dtype=np.float32), -1.0, 1.0)
    return (clipped * INT16_MAX).astype("<i2").tobytes()


def silence(seconds: float) -> bytes:
    return bytes(round(SAMPLE_RATE * seconds) * CHANNELS * SAMPLE_BYTES)
