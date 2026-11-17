# The Qwen TTS server

This page is the reference for the Qwen TTS server. It covers how the server
starts and stops, how it behaves under llama-swap, and how to run it by
hand. Related pages:
[module options](options.md#the-qwen-tts-server) and
[the HTTP API of the service](api.md).

The Qwen TTS server (`qwen-tts-server`) is a separate Python program in
`python/`. It serves Qwen3-TTS with faster-qwen3-tts on an NVIDIA GPU. Under
`tts.backend = "qwentts"`, the module configures it and llama-swap starts it.

## Startup and exit

At startup the server loads the model, prepares every voice and speaks a
warm-up sentence in each. It stops with an exit status that tells you why:

| Exit status | Cause |
|---|---|
| 1 | The model or a voice failed to load. |
| 2 | The configuration is invalid. The message names the problem. |
| 3 | A fatal GPU error, such as no free GPU memory. The request that met the error gets a 500 first. The server waits at most 2 seconds for that reply to go out. |

## Under llama-swap

The module writes an entry like this one, shown in llama-swap's YAML:

```yaml
models:
  qwen3-tts:
    cmd: qwen-tts-server --port ${PORT} --config /nix/store/…-qwen-tts-server.json
    env: ["CUDA_VISIBLE_DEVICES=0"]
    checkEndpoint: /health
    concurrencyLimit: 8
    ttl: 0
hooks:
  on_startup:
    preload: ["qwen3-tts"]
```

Three behaviours of llama-swap v268 can look like faults:

- After a successful preload, llama-swap logs
  `failed to preload model ...: status 404`. The model is loaded; ignore the
  line.
- The server's own output is not in the journal. Read it from llama-swap at
  `GET /logs/stream/upstream`.
- llama-swap restarts a server that exited only when the next request for it
  arrives. That request waits while the server loads.

## Build

The flake builds the server for x86_64 Linux only, from prebuilt PyPI
wheels: torch 2.15.0 for CUDA 13.2, faster-qwen3-tts 0.5.4 and transformers
5.15.1.

```sh
nix build .#qwen-tts-server    # the server; downloads several GB of CUDA wheels
nix build .#qwen3-tts-model    # the weights, about 4.5 GB
```

The weights are Qwen/Qwen3-TTS-12Hz-1.7B-Base at revision
`fd4b254389122332181a7c3db7f27e918eec64e3`, with a hash for each file.

## Run it by hand

```sh
qwen-tts-server --port 8930 --config qwen-tts-server.json [--host 127.0.0.1]
```

Set `CUDA_VISIBLE_DEVICES` to select the GPU. The launcher sets everything
else:

- It keeps caches in `${XDG_CACHE_HOME:-/var/cache/llama-swap}/qwen-tts-server`.
  This directory must exist, and the server's user must be able to write to
  it.
- It runs offline (`HF_HUB_OFFLINE=1`).
- It finds the GPU driver in `/run/opengl-driver/lib`.

## Configuration file

The server reads one JSON file. The module generates it from the
`tts.qwentts` options and the declared voices.

```json
{
  "model_dir": "/nix/store/…-qwen3-tts-12hz-1.7b-base",
  "language": "English",
  "chunk_size": 12,
  "queue_size": 8,
  "queue_wait_seconds": 60,
  "max_input_chars": 4096,
  "max_segment_chars": 400,
  "max_new_tokens": 600,
  "voices": {
    "narrator": {
      "clip": "/var/lib/llama-swap-voice/voices/narrator.wav",
      "transcript": "The exact words spoken in the clip.",
      "instruct": null,
      "temperature": 0.7, "top_k": 50, "top_p": 1.0, "repetition_penalty": 1.05
    },
    "guide": {
      "clip": "/var/lib/llama-swap-voice/voices/guide.wav",
      "transcript": null,
      "instruct": null,
      "temperature": 0.7, "top_k": 50, "top_p": 1.0, "repetition_penalty": 1.05
    }
  }
}
```

Every key is required. A value of the wrong type stops the server at
startup, and the message names the key. `chunk_size`, `queue_size`,
`max_input_chars`, `max_segment_chars` and `max_new_tokens` are positive
integers.

| Key | Meaning |
|---|---|
| `model_dir` | The directory of weights. |
| `language` | The language of every reply. |
| `chunk_size` | Codec steps in each streamed chunk. 12 steps are about one second of audio. |
| `queue_size` | Requests the server holds besides the one it generates. A request counts until its reply is fully sent or abandoned. More get a 503 `queue full`. |
| `queue_wait_seconds` | The longest time a request waits for its turn before a 503. Fractions are allowed. |
| `max_input_chars` | The longest text accepted. More gets a 413. The module copies the service's limit here, so both refuse the same text. |
| `max_segment_chars` | The server splits text at sentence ends into segments of at most this many characters. |
| `max_new_tokens` | Codec steps allowed for one segment. A segment that reaches the limit fails the request. |
| `voices` | The voices, by name. |
| `voices.*.clip` | The clip. See [Prepare a clip](../README.md#prepare-a-clip). |
| `voices.*.transcript` | The exact words of the clip, or `null` for embedding-only cloning. An empty string is an error. |
| `voices.*.instruct` | A style instruction, or `null`. It needs a transcript. |
| `voices.*.temperature`, `top_k`, `top_p`, `repetition_penalty` | The sampling settings of the voice. |

A voice is also a startup error if its prompt and `max_new_tokens` together
do not fit the model's context.

## Endpoints

| Endpoint | Purpose | Status codes |
|---|---|---|
| `POST /v1/tts` | Speak. The body is JSON: `{"voice": "narrator", "input": "…", "response_format": "wav"}`. | 200, 400, 413, 415, 500, 503 |
| `GET /health` | `{"status":"ready","voices":[…]}` when the server can speak. Until then `{"status":"loading"}` with `Retry-After: 5`. | 200, 503 |
| `GET /v1/voices` | `{"voices":[…]}`, also while the server loads. | 200 |

`response_format` is `wav` (default) or `pcm`. The reply streams as 24 kHz
mono 16-bit audio while the server generates it. A `wav` reply starts with a
44-byte header whose sizes are `0xFFFFFFFF`, which means "until the stream
ends". Some players then show a wrong duration, but the audio decodes
correctly.

One worker owns the GPU and serves one request at a time, in arrival order.
The server skips a queued request whose client disconnects. It stops a
request in generation within one chunk of the disconnect.

## Errors

Errors arrive before any audio, as JSON `{"error":{"message":"…"}}`. After
audio starts, a failure closes the connection. A message that quotes the
voice or the format from the request quotes at most 64 characters of it.

| Status | Cause |
|---|---|
| 400 | An unknown voice, an empty input, an unsupported format or a malformed body |
| 404, 405 | An unknown path; a wrong method. A 405 carries an `Allow` header. |
| 413 | Input over `max_input_chars`, or a body over 1 MiB |
| 415 | A body that is not `application/json` |
| 500 | Generation failed. The cause is in the server's log only. |
| 503 | The server is loading or its worker stopped (`Retry-After: 5`); the queue is full (`Retry-After: 2`); or the wait for a turn exceeded `queue_wait_seconds` |
