# The HTTP API

This page describes the HTTP API of the service: endpoints, audio formats,
errors, limits and shutdown. The API follows OpenAI's audio API. Related
pages: [module options](options.md) and
[the Qwen TTS server](qwen-tts-server.md).

Option names on this page, such as `request.timeout`, are options of the
NixOS module. [Environment variables](options.md#environment-variables)
lists the variable behind each one.

## Endpoints

The service listens on `127.0.0.1:8920` by default. Every path also works
without the `/v1` prefix.

| Endpoint | Purpose | Status codes |
|---|---|---|
| `POST /v1/audio/speech` | Text to speech | 200, 400, 500, 502, 503, 504, and any error status from llama-swap |
| `POST /v1/audio/transcriptions` | Speech to text | 200, 400, 500, 502, 503, 504, and any error status from llama-swap |
| `GET /v1/models` | The two model names | 200 |
| `GET /v1/audio/voices` | The voice names | 200 |
| `GET /health` | Health check | 200 |
| `GET /internal/liveness` | Liveness probe | 200 |
| `GET /internal/readiness` | Readiness probe | 200; 503 in plain text during shutdown |

An unknown path gets a 404 and a wrong method gets a 405.

## Speech

`POST /v1/audio/speech` takes a JSON body of at most 1 MiB.

| Field | Required | Meaning |
|---|---|---|
| `input` | yes | The text to speak, at most `tts.maxInputChars` characters (4096). |
| `voice` | no | The name of a voice. The default is `defaultVoice`. |
| `response_format` | no | `mp3` (default), `opus`, `wav` or `pcm`. |
| `speed` | no | 0.25 to 4. The default is 1. |
| `model`, `instructions` | no | Accepted and ignored. The model comes from the configuration. |

| Reply format | Encoding |
|---|---|
| `mp3` | 128 kbit/s, mono, 24 kHz |
| `opus` | Ogg Opus, 24 kbit/s, mono |
| `wav` | 16-bit, mono, 24 kHz. The reply streams, so the sizes in the header are unknown. |
| `pcm` | Raw 16-bit little-endian, mono, 24 kHz, as in OpenAI's API |

A request for `aac` or `flac` gets a 400.

A time-stretch changes the speed, so the voice keeps its pitch and the reply
still streams. At speed 1 the service does not touch the samples. At a slow
speed, a reply can end up to 50 ms short of its exact length. The service
returns a reply shorter than 20 ms without a speed change.

## Transcriptions

`POST /v1/audio/transcriptions` takes a multipart form with a `file` field.
The file can be at most 25 MiB and 20 minutes long. `response_format` is
`json` (default), `verbose_json` or `text`.

`verbose_json` returns the same `{"text": "…"}` as `json`. It carries text
only: no segments, words or timestamps.

| Upload format | Accepted |
|---|---|
| WAV | 16-bit or 24-bit PCM, or 32-bit float; mono or stereo; 8 to 48 kHz |
| Ogg Opus | Mono or stereo, one stream. Chained or multiplexed Ogg is refused. |
| MP3 | MPEG-1 or MPEG-2 Layer III, 16 to 48 kHz, mono or stereo. MPEG-2.5, at 8 to 12 kHz, is refused. |
| M4A/AAC, FLAC, WebM, Ogg Vorbis and all others | Refused with a 400 |

The service refuses M4A/AAC because it has no AAC or MP4 decoder. It refuses
FLAC because its FLAC decoder was removed: a review found defects in it on
malformed files. Convert such a file to WAV, Opus or MP3 first. Hermes does
this itself when it sees the 400.

## Errors

An error is JSON of the form `{"error":{"message":"…"}}`. This includes
unknown paths and wrong methods. After audio starts, the status cannot
change, so a later failure cuts the transfer.

| Status | Cause |
|---|---|
| 400 | A malformed body; an empty or too long `input`; an unknown voice or format; a `speed` out of range; a missing `file`; an upload that the service cannot decode or that is longer than 20 minutes |
| 404, 405 | An unknown path; a wrong method |
| 429 and other upstream errors | Passed on from llama-swap or the model server, with its message and `Retry-After` |
| 500 | A fault inside the service. The cause is in its log. |
| 502 | llama-swap failed, sent no audio, or sent a transcript over 1 MiB |
| 503 | The service is at a concurrency limit. The readiness probe also answers 503 during shutdown. |
| 504 | The request took longer than `request.timeout`, the wait at the [speech gate](options.md#concurrency) included |

### Retries

The service sends a speech request once more if it fails before any audio
arrives: the connection dropped, or llama-swap answered 502. It does not
retry after audio starts, and it does not retry a transcription. It follows
no redirect from llama-swap.

## Limits

| Limit | Value | Set by |
|---|---|---|
| Speech request body | 1 MiB | fixed |
| Speech text | 4096 characters | `tts.maxInputChars` |
| Speed | 0.25 to 4 | fixed |
| Upload size | 25 MiB | fixed |
| Upload length | 20 minutes | fixed |
| Transcript from the ASR model | 1 MiB | fixed |
| Clip size | 10 MiB | fixed |
| Time for a speech request or a transcription | 2 minutes | `request.timeout` |
| Time for any other request | 10 seconds | fixed |

## Shutdown

On SIGTERM or SIGINT the readiness probe answers 503, and the service stops
in three steps:

1. **Grace.** The service keeps serving for `SHUTDOWN_GRACE`, so that a load
   balancer can see the 503.
2. **Total.** The service accepts no new connections. Requests in progress
   get `SHUTDOWN_TOTAL` to finish.
3. **Hard.** The service cancels the requests that remain and closes every
   connection. It waits `SHUTDOWN_HARD` for them to stop. If any still run
   after that, it logs an error and exits with status 1.

[Limits and timeouts](options.md#limits-and-timeouts) lists the option for
each step.
