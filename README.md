# llama-swap-voice

Local speech-to-text and text-to-speech for Home Assistant Voice, Hermes and
any OpenAI-compatible client. It runs on the
[llama-swap](https://github.com/mostlygeek/llama-swap) you already have.

You talk to it like OpenAI's audio API. Behind it, two small Qwen3 models do
the work on your own hardware: one listens, one speaks in a voice cloned from
a short clip you provide.

> **Early days.** The Go service works today. A Nix flake with a NixOS module
> (models downloaded and wired up for you) and native Wyoming support for
> Home Assistant are next.

## How it works

```
 Home Assistant ─┐
 Hermes ─────────┼──▶ llama-swap-voice ──▶ llama-swap ──▶ llama-server
 any OpenAI app ─┘     (this repo)                         ├─ qwen3-asr  (listens)
                         │                                 └─ qwen3-tts  (speaks)
                         └─ ffmpeg: mp3, opus, flac, aac, pcm
```

1. A client sends audio to `/v1/audio/transcriptions` or text to `/v1/audio/speech`.
2. llama-swap-voice turns that OpenAI-shaped request into the one llama-server
   expects, adding the voice's reference clip for speech.
3. llama-swap starts the right model if it isn't running and passes the request on.
4. The reply streams back through ffmpeg into the format the client asked for,
   so playback starts before the whole sentence is ready.

## Quick start

You need llama-swap, a recent llama.cpp `llama-server` (v0.8.0 or later, for its
`/tts` endpoint) and ffmpeg.

**1. Add the two models to your llama-swap config.** llama-server downloads them
from Hugging Face on first start, around 4.5 GB in total.

```yaml
models:
  qwen3-asr:
    cmd: llama-server --port ${PORT} -hf ggml-org/Qwen3-ASR-1.7B-GGUF:Q8_0 -ngl 99 -c 4096 -np 1 --temp 0 --no-webui
    checkEndpoint: /health
    ttl: 0
  qwen3-tts:
    cmd: llama-server --port ${PORT} -hf ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF:Q8_0 -ngl 99 -c 4096 -np 1 --no-webui
    checkEndpoint: /health
    ttl: 0
```

`ttl: 0` keeps them loaded, so the first word isn't waiting on a model load.
Drop `-ngl 99` to run on the CPU.

**2. Give it a voice.** Any clear 5 to 15 second recording works. To try it out,
use the sample from Qwen's own docs:

```sh
mkdir -p ~/voices
curl -L -o ~/voices/sample.wav \
  https://qianwen-res.oss-cn-beijing.aliyuncs.com/Qwen3-TTS-Repo/clone.wav
```

**3. Run it.**

```sh
go install github.com/mniehe/llama-swap-voice/cmd/llama-swap-voice@latest

LLAMA_SWAP_VOICE_VOICES_DIR=~/voices \
LLAMA_SWAP_VOICE_VOICES_DEFAULT=sample \
LLAMA_SWAP_VOICE_UPSTREAM_URL=http://127.0.0.1:8080 \
llama-swap-voice
```

**4. Try it.**

```sh
curl -s localhost:8920/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"input": "The kitchen lights are off.", "response_format": "mp3"}' \
  -o hello.mp3

curl -s localhost:8920/v1/audio/transcriptions -F file=@hello.mp3
# {"text":"The kitchen lights are off."}
```

## Voices

Every `.wav` or `.mp3` in the voices directory is a voice, named after the file.

```
voices/
├── sample.wav          → voice "sample"
├── narrator.mp3        → voice "narrator"
└── alloy.mp3 -> sample.wav   → voice "alloy" (a symlink is an alias)
```

Aliases let clients that ask for a fixed voice name, like OpenAI's `alloy`,
use your own. Requests that name no voice get the default. Voices are read at
startup, so restart after adding one. A clip over 10 MiB is a startup error.
Only use recordings you have the right to clone.

The voices directory is trusted operator configuration: symlinks in it are
followed wherever they point, so only let people you trust write to it.

## Connect your apps

**Hermes** (`~/.hermes/config.yaml`):

```yaml
stt:
  provider: openai
  openai:
    base_url: http://localhost:8920/v1
    api_key: not-needed
    model: qwen3-asr
tts:
  provider: openai
  openai:
    base_url: http://localhost:8920/v1
    api_key: not-needed
    model: qwen3-tts
    voice: sample
```

The key is a placeholder: Hermes wants one, llama-swap-voice ignores it.

**Home Assistant** speaks Wyoming, not HTTP. Until native Wyoming support
lands, run the [wyoming_openai](https://github.com/roryeckel/wyoming_openai)
bridge in front of llama-swap-voice:

```sh
STT_BACKEND=OPENAI  STT_OPENAI_URL=http://localhost:8920/v1  STT_MODELS=qwen3-asr
TTS_BACKEND=OPENAI  TTS_OPENAI_URL=http://localhost:8920/v1  TTS_MODELS=qwen3-tts
TTS_VOICES=sample
```

Then add a Wyoming integration in Home Assistant pointing at the bridge.

## Options

Everything is set with environment variables. Only the default voice is required.

| Variable (`LLAMA_SWAP_VOICE_…`) | Default | What it does |
|---|---|---|
| `HOST`, `PORT` | `127.0.0.1`, `8920` | Where to listen |
| `UPSTREAM_URL` | `http://127.0.0.1:8080` | Your llama-swap |
| `UPSTREAM_TTSMODEL`, `UPSTREAM_ASRMODEL` | `qwen3-tts`, `qwen3-asr` | Model names in your llama-swap config |
| `UPSTREAM_TTSCONCURRENCY` | `1` | Speech requests sent to the TTS model at once; see below |
| `VOICES_DIR` | `/var/lib/llama-swap-voice/voices` | Where voice clips live |
| `VOICES_DEFAULT` | *required* | Voice used when a request names none |
| `TTS_LANG` | `en` | Language passed to the speech model |
| `TTS_MAXINPUTCHARS` | `4096` | Longest text accepted per request |
| `ASR_TRANSCODEINPUT` | `true` | Decode every upload, WAV included, to 16 kHz mono WAV before transcribing, and reject audio over 20 minutes. `false` forwards uploads untouched: no format conversion and no length limit |
| `ASR_PREFIXREGEX` | `^language \w+<asr_text>` | Removed from the start of each transcript |
| `FFMPEG` | `ffmpeg` | ffmpeg binary |
| `REQUEST_TIMEOUT` | `2m` | Longest a speech or transcription request may take, streaming included. Every other endpoint gets 10 seconds |
| `REQUEST_MAXCONCURRENT` | `4` | Requests handled at once; more get a 503 |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | `debug`/`info`/`warn`/`error`; `json`/`console` |
| `SHUTDOWN_GRACE`, `SHUTDOWN_TOTAL`, `SHUTDOWN_HARD` | `0s`, `15s`, `3s` | How shutdown unfolds; see below |

If anything is wrong at startup, it lists every problem at once and exits.

**Concurrent requests.** Speech requests beyond `UPSTREAM_TTSCONCURRENCY`
wait their turn in arrival order, for up to `REQUEST_TIMEOUT`, and still count
towards `REQUEST_MAXCONCURRENT`. One that is still waiting when the timeout
ends gets a 504. `UPSTREAM_TTSCONCURRENCY` must not exceed llama-swap's
`concurrencyLimit` or llama-server's `-np` for the TTS model.

On SIGTERM or SIGINT, readiness turns 503 and shutdown runs in three steps:

1. **Grace**: keep serving while load balancers notice readiness.
2. **Total**: stop accepting connections and let in-flight requests finish.
3. **Hard**: if any are still running, cancel them, close every connection,
   stop their ffmpeg processes and wait this long for them to wind down.
   `SHUTDOWN_HARD` must be at least `1s`, the time ffmpeg gets between
   SIGTERM and SIGKILL. If requests are still running after it, the service
   logs an error and exits with status 1.

## API

Every path also works without the `/v1` prefix.

| Endpoint | Does |
|---|---|
| `POST /v1/audio/speech` | Text to speech. JSON: `input`, `voice`, `response_format` (`mp3` default, `opus`, `aac`, `flac`, `wav`, `pcm`), `speed` (0.25 to 4) |
| `POST /v1/audio/transcriptions` | Speech to text. Multipart `file` (up to 25 MB, and 20 minutes unless `ASR_TRANSCODEINPUT=false`), `response_format` `json`, `verbose_json` or `text` |
| `GET /v1/models`, `GET /v1/audio/voices` | What's available |
| `GET /health`, `GET /internal/liveness`, `GET /internal/readiness` | Health checks; readiness turns 503 while shutting down |

`pcm` is raw 16-bit little-endian mono at 24 kHz, as in OpenAI's API. The
`model` field is accepted and ignored; the models come from your config.

Errors, unknown paths and wrong methods included, come back as JSON:
`{"error":{"message":"…"}}`. A transcript over 1 MiB from the model is a 502.

## Development

```sh
go test ./...                     # no GPU, models or ffmpeg needed
golangci-lint run ./...
```

With ffmpeg on `PATH` or in `LLAMA_SWAP_VOICE_FFMPEG`, the tests also check
that real ffmpeg streams every output format before its input ends.

## Licence

MIT. The Qwen3 models are Apache-2.0, published by the Qwen team and
converted to GGUF by [ggml-org](https://huggingface.co/ggml-org).
