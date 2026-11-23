# The HTTP API

This page describes the HTTP API of the service: endpoints, audio formats,
errors, limits and shutdown. The API follows OpenAI's audio API. The last
section describes the optional [Wyoming](#wyoming) listener. Related pages:
[module options](options.md) and [the Qwen TTS server](qwen-tts-server.md).

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
still streams. At speed 1 the service does not touch the samples. At any
other speed, the exact length is the upstream audio's length divided by the
speed. From upstream audio of at least 20 ms, a reply can end up to 10 ms
short of it, and below speed 0.5 up to 10 ms / speed - 10 ms short (30 ms at
speed 0.25). Upstream audio shorter than 20 ms is not stretched but cut to the
exact length, so a slow speed returns all of it and a fast speed its start.
These lengths are of the samples before encoding; `mp3` adds the encoder's
delay and padding.

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

## Wyoming

With `WYOMING_ENABLED=true` the service also speaks the
[Wyoming protocol](https://github.com/OHF-Voice/wyoming) over TCP, so that
Home Assistant can use it for speech to text and text to speech without a
bridge. The listener binds `WYOMING_HOST` and `WYOMING_PORT`
(`127.0.0.1:10300` by default) before the readiness probe answers. If the
bind fails, the service does not start. If accepting a connection fails
because the system is short of file descriptors, buffers or memory, the
listener logs a warning and tries again after a pause that grows from 5 ms
to 1 s, and the service keeps running.
[Environment variables](options.md#environment-variables) lists the
`WYOMING_` settings, and [Wyoming](options.md#wyoming) the module options
that set them. Both listeners share one pipeline, so a Wyoming request
takes the same transcription or speech slot as an HTTP request.

### Events

| Event from the client | What the service does |
|---|---|
| `describe` | Sends `info`. |
| `ping` | Sends `pong` with the same `text`. |
| `transcribe` | Accepted and otherwise ignored. Its `language` is not read. |
| `audio-start` | Opens a recording and takes a transcription slot. |
| `audio-chunk` | Adds the payload to the recording. |
| `audio-stop` | Transcribes the recording and sends `transcript`. |
| `synthesize` | Speaks `text` in `voice.name`, or in the default voice when no name is given, at `WYOMING_SPEED`. Sends `audio-start`, `audio-chunk` events and `audio-stop`. Inside a stream it is ignored. |
| `synthesize-start` | Opens a [stream](#streamed-synthesis) in `voice.name`, or in the default voice. |
| `synthesize-chunk` | Adds `text` to the stream. In `sentences` mode, speaks each sentence it completes. |
| `synthesize-stop` | Speaks the rest of the stream's text. Sends `audio-stop` if audio was sent, then `synthesize-stopped`. |

The service ignores every other event. It also ignores `voice.speaker`,
`voice.language`, `text_format` and `context` in `synthesize` and
`synthesize-start`. Each event is read whole, within the frame limits, before
it is dropped.

A connection is in one of four states:

| State | Starts with | Accepts | Refuses |
|---|---|---|---|
| Idle | the connection; the end of a reply or a stream | `describe`, `ping`, `transcribe`, `audio-start`, `synthesize`, `synthesize-start` | `audio-chunk`, `audio-stop`, `synthesize-chunk`, `synthesize-stop` |
| Recording | `audio-start` | `describe`, `ping`, `audio-chunk`, `audio-stop` | `transcribe`, `audio-start`, `synthesize`, `synthesize-start`, `synthesize-chunk`, `synthesize-stop` |
| Synthesising | `synthesize` | nothing: the service reads no event until the reply ends | — |
| Streaming | `synthesize-start` | `describe`, `ping`, `synthesize-chunk`, `synthesize-stop`, and `synthesize`, which it ignores | `synthesize-start`, `transcribe`, `audio-start`, `audio-chunk`, `audio-stop` |

A known event in the wrong state gets an `error` with code `invalid`, and the
connection closes. A connection can carry any number of requests and streams,
one at a time.

### The info event

`info` lists one ASR program and one TTS program, both named
`llama-swap-voice` with the service's version.

- The ASR program's description is the ASR model's name. It has one model,
  that name, with the `WYOMING_LANGUAGES` languages.
  `supports_transcript_streaming` is `false`.
- The TTS program's description is the TTS model's name. It lists every
  voice, each with the `WYOMING_LANGUAGES` languages. The default voice comes
  first, and its description ends with ` (default)`.
  `supports_synthesize_streaming` is `true` when `WYOMING_STREAMING` is
  `sentences` and `false` when it is `whole`.

### Streamed synthesis

Home Assistant streams the reply of a language model as `synthesize-start`,
`synthesize-chunk` events, a `synthesize` with the whole text for servers that
do not stream, and `synthesize-stop`. The service ignores that `synthesize`
and answers the stream with one `audio-start`, the `audio-chunk` events, one
`audio-stop` and `synthesize-stopped`. `WYOMING_STREAMING` selects the mode:

- `sentences`. The service speaks each sentence as soon as the text after it
  has arrived, so audio starts before the reply is complete. A sentence ends
  at `.`, `!`, `?`, `…` or a run of them followed by whitespace, or at a
  newline. The sentences completed by one `synthesize-chunk` are joined while
  the joined text stays within `WYOMING_STREAMINGMAXCHARS` characters, and a
  longer sentence is spoken whole. `synthesize-stop` speaks the rest.
- `whole`. The service collects the text and speaks it all once
  `synthesize-stop` arrives.

A stream holds no speech slot. Each sentence, or the whole text, is admitted
alone: it takes a speech slot and its own request deadline and gives both
back when its audio is sent. A sentence can therefore fail with `busy` in the
middle of a reply. The text not yet spoken plus an arriving chunk may not
exceed `tts.maxInputChars` characters, whitespace included; a chunk over it
is refused with `invalid` before any of it is spoken. No event is read and no
sentence starts after `maxStreamDuration`, 10 minutes from
`synthesize-start`; a sentence already running finishes within its own
request timeout. While the stream waits for text, each event must arrive
within `idleTimeout`. The service reads no event while it speaks a sentence,
so events sent meanwhile wait in the socket.

### Audio formats

| Direction | Format |
|---|---|
| Recording, from the client | PCM, 2 or 3 bytes a sample (16- or 24-bit), 1 or 2 channels, 8000 to 48000 Hz. Each `audio-chunk` must repeat the format of `audio-start`. |
| Speech, to the client | PCM, `rate` 24000, `width` 2, `channels` 1. Each `audio-chunk` holds at most 4800 bytes (100 ms) and whole samples. |

### Limits

| Limit | Value | Set by |
|---|---|---|
| Header line of one event | 8 KiB | fixed |
| Data of one event | 256 KiB | fixed |
| Payload of one event | 1 MiB | fixed |
| Open connections | 64; the service closes more at once | fixed |
| Recording, with the 44-byte WAV header the service adds | 25 MiB | fixed, the upload size |
| Speech text, and the text of a stream not yet spoken plus an arriving chunk | 4096 characters | `tts.maxInputChars` |
| `maxStreamDuration`, from `synthesize-start`: no event is read and no sentence starts after it | 10 minutes | fixed |
| `idleTimeout` | 60 seconds | fixed |
| `writeTimeout` | 10 seconds | fixed |
| Time for a request | 2 minutes | `request.timeout` |

The two timeouts work as follows:

- `idleTimeout`. A connection with no request or stream open must start one
  within 60 seconds of its accept or of the end of its last reply or stream.
  `describe`, `ping` and unknown events do not extend this time. While a
  recording or a stream is open, each event must arrive within 60 seconds,
  and the recording must end by the request deadline, the stream by
  `maxStreamDuration`. A stream that waits longer for an event closes without
  a reply.
- `writeTimeout`. Each event of a reply (`transcript`, `audio-start`, each
  `audio-chunk`, `audio-stop`, `synthesize-stopped` and `error`) gets
  10 seconds to be written. `info` and `pong` get the same, cut short when no
  request is open by the stream's end or else by the idle deadline.
- The request deadline starts at `audio-start`, at `synthesize` or when a
  stream's sentence is admitted. It bounds the recording, the call to
  llama-swap and the audio that follows: once it passes, the service sends no
  further chunk.

### Errors

An `error` event has `text` and `code`, and the connection closes after it.
This includes a failure after `audio-start` of a synthesis reply. Inside a
stream, `synthesize-stopped` follows the `error`. A refused
`synthesize-start`, for malformed data or during shutdown, opens no stream
and gets the `error` only.

| Code | Cause |
|---|---|
| `invalid` | A malformed event or one over a limit; a known event in the wrong state; an unsupported audio format; an empty or too long text; an unknown voice, with the list of voices |
| `busy` | Every transcription or speech slot is taken, also for a sentence in the middle of a stream |
| `timeout` | The request took longer than `request.timeout`, or the stream reached `maxStreamDuration` |
| `upstream` | llama-swap failed, sent no audio or broke off its audio |
| `shutting-down` | The service is shutting down |
| `internal` | A fault inside the service. The cause is in its log. |

### Disconnects

While the service waits for llama-swap, it does not read the socket. It
therefore notices a client that has gone at its next write or at the request
deadline, not at once. A write failure or the request deadline ends the
request and frees its slot. A client that sends a request and then reads
nothing blocks the reply, and `writeTimeout` ends that request.

### Shutdown and the Wyoming listener

The Wyoming listener follows the [shutdown](#shutdown) steps of the HTTP
server:

1. **Grace.** Both listeners keep serving.
2. **Total.** Both listeners stop accepting. The service closes each Wyoming
   connection that has no request or stream open. A connection with an open
   request keeps it until its reply is written, and one with an open stream
   keeps it until `synthesize-stop` or the stream's end; then it closes.
   Events that the service had already read are handled first: a new request
   or stream among them gets an `error` with code `shutting-down`. A request
   that arrives later finds the connection closed.
3. **Hard.** The service cancels the requests that remain, sends
   `shutting-down` where it still can, and closes every connection. It waits
   `SHUTDOWN_HARD` for the requests and the connections to stop.
