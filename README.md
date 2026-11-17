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
                         └─ encodes mp3, opus, wav, pcm and changes speed itself
```

1. A client sends audio to `/v1/audio/transcriptions` or text to `/v1/audio/speech`.
2. llama-swap-voice turns that OpenAI-shaped request into the one llama-server
   expects, adding the voice's reference clip for speech.
3. llama-swap starts the right model if it isn't running and passes the request on.
4. The reply is encoded into the format the client asked for as it arrives,
   so playback starts before the whole sentence is ready.

## Quick start

You need llama-swap and a recent llama.cpp `llama-server` (v0.8.0 or later, for
its `/tts` endpoint). Building needs a C compiler, pkg-config and libopus,
because Opus output is encoded with libopus.

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
| `UPSTREAM_TTSBACKEND` | `llamacpp` | How speech requests reach the TTS model. `llamacpp` posts a multipart form to `/tts` with the voice's clip attached, for llama-server. `qwentts` posts JSON `{"voice","input","response_format":"wav"}` to `/v1/tts`, naming the voice and sending no clip, for a TTS server that holds the voices itself. Each voice still needs a clip in `VOICES_DIR` to be accepted |
| `UPSTREAM_TTSCONCURRENCY` | `1` | Speech requests sent to the TTS model at once; see below |
| `VOICES_DIR` | `/var/lib/llama-swap-voice/voices` | Where voice clips live |
| `VOICES_DEFAULT` | *required* | Voice used when a request names none |
| `TTS_LANG` | `en` | Language passed to the speech model |
| `TTS_MAXINPUTCHARS` | `4096` | Longest text accepted per request |
| `ASR_PREFIXREGEX` | `^language \w+<asr_text>` | Removed from the start of each transcript |
| `REQUEST_TIMEOUT` | `2m` | Longest a speech or transcription request may take, streaming included. Every other endpoint gets 10 seconds |
| `REQUEST_MAXCONCURRENT` | `4` | Transcriptions handled at once; more get a 503 |
| `REQUEST_MAXCONCURRENTTTS` | `8` | Speech requests handled at once, queued ones included; more get a 503 |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | `debug`/`info`/`warn`/`error`; `json`/`console` |
| `SHUTDOWN_GRACE`, `SHUTDOWN_TOTAL`, `SHUTDOWN_HARD` | `0s`, `15s`, `3s` | How shutdown unfolds; see below |

If anything is wrong at startup, it lists every problem at once and exits.

**Concurrent requests.** Speech requests count against
`REQUEST_MAXCONCURRENTTTS` and transcriptions against `REQUEST_MAXCONCURRENT`,
so queued speech never turns a transcription away. Each gets a 503 beyond its
own limit. An admitted speech request beyond `UPSTREAM_TTSCONCURRENCY` waits
its turn in arrival order, for up to `REQUEST_TIMEOUT`, and keeps its
`REQUEST_MAXCONCURRENTTTS` place while it waits. One that is still waiting
when the timeout ends gets a 504. `UPSTREAM_TTSCONCURRENCY` must not exceed
llama-swap's `concurrencyLimit` or llama-server's `-np` for the TTS model.

**Retries.** When a speech request to the TTS model fails before any audio
comes back, because the connection dropped or llama-swap answered 502 while
the model was down, the service sends the same request once more, keeping its
place in the TTS queue. It never retries once audio has started, never more
than once, and never for a transcription, and it follows no upstream redirect.
Any other upstream error reaches the client with the upstream's status and
message, and its `Retry-After`, if any.

On SIGTERM or SIGINT, readiness turns 503 and shutdown runs in three steps:

1. **Grace**: keep serving while load balancers notice readiness.
2. **Total**: stop accepting connections and let in-flight requests finish.
3. **Hard**: if any are still running, cancel them, close every connection
   and wait this long for them to wind down. If requests are still running
   after it, the service logs an error and exits with status 1.

## NixOS module

The flake's `nixosModules.default` runs the service as a systemd unit under
its own user. It does not set up llama-swap or the models yet.

```nix
{
  imports = [llama-swap-voice.nixosModules.default];

  services.llama-swap-voice = {
    enable = true;
    defaultVoice = "sample";
    voices.sample.clip = "/srv/voices/sample.wav";
  };
}
```

Options under `services.llama-swap-voice`:

| Option | Type | Default | What it does |
|---|---|---|---|
| `enable` | bool | `false` | Run the service |
| `package` | package | this flake's package | The build to run |
| `user`, `group` | string | `llama-swap-voice` | Account the service runs as. The module creates them only under the default name; a custom user or group must already exist |
| `stateDir` | absolute path, not `/`, no trailing slash, no `%` | `/var/lib/llama-swap-voice` | The service reads voices from `<stateDir>/voices`. It must belong to this module alone, because tmpfiles sets its owner and mode |
| `defaultVoice` | string | *required* | `VOICES_DEFAULT`; must be one of `voiceNames` |
| `host`, `port` | string, integer 1 to 65535 | `127.0.0.1`, `8920` | `HOST`, `PORT` |
| `upstreamURL` | `http://` or `https://` URL | `http://127.0.0.1:8080` | `UPSTREAM_URL`. Evaluation checks only for the scheme, a non-empty host and no whitespace; the service parses the URL fully at startup |
| `ipAddressAllow` | list of strings | `[]` | Addresses besides loopback the service may talk to, in systemd's `IPAddressAllow=` syntax |
| `tts.backend` | `llamacpp` or `qwentts` | `llamacpp` | `UPSTREAM_TTSBACKEND` |
| `tts.maxInputChars` | positive int or null | `null` | `TTS_MAXINPUTCHARS` |
| `upstream.ttsConcurrency` | positive int or null | `null` | `UPSTREAM_TTSCONCURRENCY` |
| `request.timeout` | positive Go duration or null | `null` | `REQUEST_TIMEOUT` |
| `request.maxConcurrent`, `request.maxConcurrentTTS` | positive int or null | `null` | `REQUEST_MAXCONCURRENT`, `REQUEST_MAXCONCURRENTTTS` |
| `shutdown.grace` | Go duration or null | `null` | `SHUTDOWN_GRACE` |
| `shutdown.total`, `shutdown.hard` | positive Go duration or null | `null` | `SHUTDOWN_TOTAL`, `SHUTDOWN_HARD` |
| `voices.<name>.clip` | absolute path, as a string | *required* | The voice's recording; `.wav` or `.mp3`, and `.wav` under `qwentts` |
| `voices.<name>.transcript` | string or null | `null` | What the clip says. Null selects embedding-only cloning, which keeps the voice but not its accent. No effect yet; see below |
| `voices.<name>.instruct` | string or null | `null` | Style instruction; under `qwentts` it needs a transcript. No effect yet; see below |
| `voices.<name>.temperature`, `.topK`, `.topP`, `.repetitionPenalty` | number, int, number, number | `0.7`, `50`, `1.0`, `1.05` | Sampling settings. No effect yet; see below |
| `voiceNames` | list of strings, read-only | | The declared voice names, sorted, since Nix attribute sets keep no order |
| `extraEnvironment` | attribute set of strings | `{}` | Extra variables by full name; they win over generated ones |

A limit left `null` leaves its variable unset, so the service's own default
from the table above applies. Durations use Go's syntax, such as `1m30s`,
without a leading `+` or `-`; evaluation reads them exactly as the service
does and refuses any above Go's maximum of about 292 years. Evaluation fails, naming the option, when
`defaultVoice` is missing or undeclared, a clip path is relative or has
another extension, or a `qwentts` voice has an empty transcript or an
`instruct` without a transcript.

A voice's `transcript`, `instruct` and sampling settings are for the
faster-qwen3-tts server's configuration. This module does not generate that
configuration yet, so these values have no effect on the service today; only
the clip does.

Stopping the unit waits for the shutdown steps one after another, so
`TimeoutStopSec` is the sum of the shutdown durations you set plus 30 seconds
for the ones you leave unset, and at least 90 seconds. A `SHUTDOWN_GRACE`,
`SHUTDOWN_TOTAL` or `SHUTDOWN_HARD` set through `extraEnvironment` counts
instead of the option it overrides, and evaluation fails, naming the
variable, if it is not a duration the option would accept.

Each voice becomes the symlink `<stateDir>/voices/<name>.<extension>`,
pointing at its clip. Clips stay where they are and never enter the Nix
store, and the service user must be able to read them. Clips copied into the
directory by hand also count and are never removed. The module does not
remove links either: after you delete a voice from the configuration, delete
its link by hand. If a declared link points somewhere other than its clip,
for example after you change the clip's path, the service refuses to start
until you delete the link and run `systemd-tmpfiles --create`. If you change
a clip's extension, including only its case, the old link stays beside the
new one and the service refuses to start with `voice "<name>" is provided by
both`; delete the link with the old extension by hand.

The unit can write only to its private `/tmp`, where large uploads spool,
cannot read home directories, and can exchange packets only with loopback. Put a non-loopback upstream, its DNS resolver,
or the clients of a non-loopback `host` in `ipAddressAllow`.

## API

Every path also works without the `/v1` prefix.

| Endpoint | Does |
|---|---|
| `POST /v1/audio/speech` | Text to speech. JSON: `input`, `voice`, `response_format` (`mp3` default, `opus`, `wav`, `pcm`; `aac` and `flac` are a 400 `unsupported response_format`), `speed` (0.25 to 4, see below) |
| `POST /v1/audio/transcriptions` | Speech to text. Multipart `file` (up to 25 MB and 20 minutes) in WAV (16-bit, 24-bit or float), Ogg Opus or MP3, mono or stereo at 8 to 48 kHz, decoded in Go to 16 kHz mono before transcribing. Anything else, FLAC, M4A, WebM, AAC and chained Ogg included, is a 400 `could not decode audio: unsupported audio format`; a damaged or empty file is a 400 `could not decode audio: corrupted audio`, and audio over 20 minutes a 400 `audio is longer than 20 minutes`. `response_format` `json`, `verbose_json` or `text` |
| `GET /v1/models`, `GET /v1/audio/voices` | What's available |
| `GET /health`, `GET /internal/liveness`, `GET /internal/readiness` | Health checks; readiness turns 503 while shutting down |

`pcm` is raw 16-bit little-endian mono at 24 kHz, as in OpenAI's API. `mp3`
is 128 kbit/s mono at 24 kHz and `opus` is Ogg Opus at 24 kbit/s mono. The
`model` field is accepted and ignored; the models come from your config.

`speed` is applied by a built-in time-stretch (WSOLA), so the voice keeps its
pitch and the reply still streams: audio starts after a few tens of
milliseconds of speech rather than at the end. At speed 1 the samples are
untouched. At slow speeds a reply can end up to 50 ms short of the exact
stretched length, and one shorter than 20 ms is returned unstretched.

Errors, unknown paths and wrong methods included, come back as JSON:
`{"error":{"message":"…"}}`. A transcript over 1 MiB from the model is a 502.

## Development

```sh
nix-shell -p libopus pkg-config gcc opus-tools ffmpeg-headless   # or install them yourself
go test ./...                     # no GPU or models needed
golangci-lint run ./...
```

The service itself runs no external programs. opus-tools and ffmpeg are
optional test oracles: some tests use `opusinfo` and `opusdec` from
opus-tools and ffmpeg to decode the encoded replies independently, and skip
those checks when the tools are missing.

## Licence

MIT. Binaries built from it also include
[shine-mp3](https://github.com/braheezy/shine-mp3), which is LGPL-2.0, and
link [libopus](https://opus-codec.org), which is BSD-3-Clause. The Qwen3
models are Apache-2.0, published by the Qwen team and converted to GGUF by
[ggml-org](https://huggingface.co/ggml-org).
