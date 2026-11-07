# Test fixtures

Two seconds of speech, cut once from a 24 kHz mono float WAV of TTS output
(`speech.wav` from the ffmpeg probes, 8.08 s), then encoded with the reference
tools from nixpkgs:

```sh
nix shell nixpkgs#ffmpeg-headless nixpkgs#opus-tools nixpkgs#lame
```

ffmpeg 9.0.2, opusenc 0.2 (libopus 1.6.1), LAME 4.0. Each command
was run from this directory with `SRC` pointing at the source WAV.

| File | Format | Command |
|---|---|---|
| `speech.wav` | PCM s16, 24 kHz mono | `ffmpeg -nostdin -y -loglevel error -ss 1 -t 2 -i "$SRC" -map_metadata -1 -fflags +bitexact -flags:a +bitexact -c:a pcm_s16le -ar 24000 -ac 1 speech.wav` |
| `speech.opus` | Ogg Opus, mono, 32 kbit/s | `opusenc --quiet --bitrate 32 speech.wav speech.opus` |
| `speech-stereo.opus` | Ogg Opus, stereo, 48 kbit/s | `ffmpeg -nostdin -y -loglevel error -i speech.wav -map_metadata -1 -fflags +bitexact -flags:a +bitexact -ac 2 -c:a libopus -b:a 48k speech-stereo.opus` |
| `speech.mp3` | MPEG-2 Layer III, 24 kHz mono, 64 kbit/s | `lame --quiet -b 64 speech.wav speech.mp3` |
| `speech.m4a` | AAC-LC in MP4, 48 kbit/s; must be rejected | `ffmpeg -nostdin -y -loglevel error -i speech.wav -map_metadata -1 -fflags +bitexact -flags:a +bitexact -c:a aac -b:a 48k speech.m4a` |

ffmpeg decodes both Opus files to exactly 96,000 samples at 48 kHz, which is
why the tests expect exactly 32,000 frames at 16 kHz after pre-skip and end
trimming.

The 1300-second 8 kHz WAV for the duration limit is generated in the tests
rather than committed.
