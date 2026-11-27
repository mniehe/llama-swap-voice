# The HTTP API

This page describes the HTTP API of the service: endpoints, audio formats,
errors, limits and shutdown. The API follows OpenAI's audio API. The last
section describes the optional [Wyoming](#wyoming) listener. Related pages:
[module options](options.md) and [the Qwen TTS server](qwen-tts-server.md).

Option names on this page, such as `request.timeout`, are options of the
NixOS module. [Environment variables](options.md#environment-variables)
lists the variable behind each one.

## Endpoints

The service listens on `127.0.0.1:8920` by default. Every `/v1` path also
works without the `/v1` prefix. The `/recordings` paths exist only with
`recordings.enable` on, and have no `/v1` form.

| Endpoint | Purpose | Status codes |
|---|---|---|
| `POST /v1/audio/speech` | Text to speech | 200, 400, 500, 502, 503, 504, and any error status from llama-swap |
| `POST /v1/audio/transcriptions` | Speech to text | 200, 400, 500, 502, 503, 504, and any error status from llama-swap |
| `GET /v1/models` | The two model names | 200 |
| `GET /v1/audio/voices` | The voice names | 200 |
| `GET /health` | Health check | 200 |
| `GET /internal/liveness` | Liveness probe | 200 |
| `GET /internal/readiness` | Readiness probe | 200; 503 in plain text during shutdown |
| `GET /recordings` | The [recordings](#recordings) page, in HTML | 200, 400, 403, 500 |
| `GET /recordings/{id}` | The page of one recording, in HTML | 200, 403, 404, 500 |
| `POST /recordings/{id}` | Save the review of one recording | 303, 400, 403, 404, 500 |
| `GET /recordings/{id}/audio` | One recording, as Ogg Opus | 200, 206, 304, 403, 404, 412, 416, 500 |
| `POST /recordings/{id}/keep` | Mark a recording kept, or release it | 303, 400, 403, 404, 409, 500 |
| `GET /recordings/profiles` | The speaker profiles page, in HTML | 200, 403, 500 |
| `POST /recordings/profiles` | Add a speaker profile | 303, 400, 403, 409, 500 |
| `POST /recordings/profiles/{profile}/rename` | Rename a speaker profile | 303, 400, 403, 404, 409, 500 |
| `POST /recordings/profiles/{profile}/delete` | Delete a speaker profile | 303, 400, 403, 404, 500 |

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
other speed, upstream audio of n samples is stretched to n / speed samples,
rounded down. The reply is never longer than that and can end up to 10 ms
short of it. Upstream audio shorter than 20 ms, or whose n / speed is shorter
than 10 ms, is not stretched: the reply is its first n / speed samples, or all
n samples if that is fewer. So a slow speed returns all of it and a fast speed
its start. Upstream audio of fewer samples than the speed gives no samples,
and the request gets a 502. These lengths are of the samples before encoding;
`mp3` adds the encoder's delay and padding.

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

## Recordings

With `recordings.enable` on, the service keeps the audio and transcript of
every successful transcription. [Recordings](options.md#recordings) in the
module options describes what it stores, where, and for how long. Without
the option, these paths answer 404 like any unknown path. They have no
authentication; the recordings hold household speech, so limit who can reach
the port.

The recordings paths answer only a request whose `Host` header names an allowed
host: `127.0.0.1`, `[::1]` or `localhost` on any port, the listen address
`host`, or an entry of `recordings.allowedHosts`. Any other `Host` gets a
403, which stops a DNS-rebinding page in a browser on your network from
reading the recordings. The service never reads `X-Forwarded-Host`, so a
reverse proxy must pass the public `Host` through unchanged, and that host
must be in `recordings.allowedHosts`.

`GET /recordings` returns an HTML page that needs no JavaScript. It lists the
newest 100 recordings, newest first, each with its UTC time, its length, its
source (`openai` or `wyoming`), the names of its speakers and the first
2 KiB of its transcript, cut at a character and marked with an ellipsis when
longer, and says how many of all the recordings it shows. Each row links to
the recording's page and has an audio player and a button that marks the
recording kept or releases it. A row flagged with a transcription error is
marked "Transcription error" and has no button, because such a recording
stays kept. When older recordings exist, an "Older" link leads to `GET /recordings?before={id}`, the next 100 recordings
older than `{id}`. When the recording `{id}` has been deleted since, the
page starts at the end of the second in its id, so a recording made in that
second may show on both pages. A `before` that is not one recording id with
a real date and time, or a query with a malformed escape such as `%zz`, gets
a 400. The page sends `Content-Security-Policy: frame-ancestors 'none'`, so
no other site can frame it.

`GET /recordings/{id}/audio` returns the recording as `audio/ogg`: Ogg Opus,
16 kHz mono, 32 kbps. It serves byte ranges (206, or 416 for a range past
the end or a `Range` it cannot parse), `HEAD` and conditional requests (304,
or 412 for an `If-Match` or `If-Unmodified-Since` that fails). It ignores a
`Range` that names no range (`bytes=`), more than 200 ranges, or ranges that
together exceed the file, and answers 200 with the whole file. Every reply to
a `GET` or `HEAD` of this path carries `Cache-Control: no-store`, errors
included: the 403 for a `Host` that is not allowed, the 404, and every other
status, except a 503 for shutdown, which the service answers before it
routes the request. A reply to another method, the 405, does not carry it.
A 412, 416 or 500 is a JSON error. A 416 for a range past the end also
carries `Content-Range: bytes */{size}`; a 416 for a `Range` it cannot parse
carries no `Content-Range`. The transfer may take as long as sending the
file at 16 KiB/s takes, and at least 10 seconds.

`GET /recordings/{id}` returns the page of one recording, in HTML that needs
no JavaScript: its time, length and source, an audio player, its whole
transcript, and a form that reviews it, with a checkbox for each speaker
profile. Like the list, it is rendered whole
before it is sent and carries `Cache-Control: no-store` and
`Content-Security-Policy: frame-ancestors 'none'`.

`POST /recordings/{id}` saves that form, an ordinary
`application/x-www-form-urlencoded` form:

| Field | Meaning |
|---|---|
| `keep` | `true` keeps the recording; absent releases it. |
| `transcriptionError` | `true` flags the transcript as wrong; absent clears the flag. |
| `correctText` | What was said, at most 16 KiB of UTF-8. Empty, with the flag, means the recogniser wrote text when nobody spoke. |
| `profile` | The id of a speaker profile, once for each speaker. The recording's speakers become exactly these. |

A ticked checkbox is sent as `true` and an unticked one not at all, as a
browser does. A `profile` given twice counts once. A flagged recording is always kept, whatever `keep` says, so
the 7-day expiry never deletes a test case. Without the flag, the service
stores no correct text, whatever `correctText` says. The original transcript
is never changed. On success the post answers 303 with
`Location: /recordings/{id}`. A checkbox with a value other than `true`, a
field other than `profile` given twice, a correct text over 16 KiB or not
valid UTF-8, a `profile` that names no profile, or a form over 51,776 bytes
gets a 400. That bound fits the correct text at its limit, percent-encoded,
and all 64 profiles. The service ignores fields it does not know.

`POST /recordings/{id}/keep` takes a form with one field, `keep`, set to
`true` or `false` and given once. On success it answers 303 with
`Location: /recordings`. Any other value, a missing or repeated field, or a
form over 1 KiB gets a 400. Releasing a recording flagged with a
transcription error gets a 409; clear the flag on its page first.

`GET /recordings/profiles` returns the speaker profiles page, in HTML that
needs no JavaScript, with the same headers as the list. A profile is a person
whom recordings can be assigned to. The page lists the profiles in the order
they were added, each with a form that renames it and a button that deletes
it, and has a form that adds one. Each form posts one field, `name`, given
once, except the delete form, which posts none:

- `POST /recordings/profiles` adds a profile named `name`.
- `POST /recordings/profiles/{profile}/rename` renames the profile
  `{profile}`. Every recording assigned to it shows the new name.
- `POST /recordings/profiles/{profile}/delete` deletes the profile
  `{profile}`. Its recordings stay; they no longer show it.

On success each answers 303 with `Location: /recordings/profiles`. The
service trims spaces from a name. A name that is then empty, over 64 bytes,
not valid UTF-8 or holds a control character, a missing or repeated `name`,
a 65th profile, or a form over 1 KiB gets a 400. A name another profile has,
ignoring case, gets a 409. A `{profile}` that is not 16 hex digits or names
no profile gets a 404. The service gives each profile its id; the pages show
only its name.

A post to any of these forms that a browser marks as coming from another
site (`Sec-Fetch-Site`, or an `Origin` that does not match the host) gets a
403.

An id that does not have the form of a recording id, such as
`20261125T153000Z-1a2b3c4d`, gets a 404 without touching the disk, as does
an id with no recording. `profiles` is never taken for a recording id.

The service deletes a recording that is not kept 7 days after it was made,
checking once at start and then every hour. All recordings together may take
at most 1 GiB. A new recording first displaces the oldest ones that are not
kept. Kept recordings count toward the 1 GiB, and so do the bytes no
deletion can reclaim: recordings left on disk because their metadata or
files cannot be read, files the service failed to delete, and the speaker
profiles file. Neither a review nor a profile change is refused for the cap;
the bytes they add count at the next save. When those leave no room for a new recording, the service saves nothing new, logs a
warning, and still returns the transcript.

The service saves a recording in the background after it has returned the
transcript, one at a time in the order they arrived. A recording is dated
when its transcription finished, not when it is saved. Up to 64 MiB may wait
to be saved, counting each recording's audio, its transcript and 512 bytes
more. A recording that would pass that is dropped with a warning that names
the request. See [shutdown](#shutdown) for what happens to waiting recordings
when the service stops.

## Errors

An error is JSON of the form `{"error":{"message":"…"}}`. This includes
unknown paths and wrong methods. After audio starts, the status cannot
change, so a later failure cuts the transfer.

| Status | Cause |
|---|---|
| 400 | A malformed body; an empty or too long `input`; an unknown voice or format; a `speed` out of range; a missing `file`; an upload that the service cannot decode or that is longer than 20 minutes; a malformed recordings page query; a recordings form that breaks a bound |
| 403 | A recordings request whose `Host` is not allowed; a recordings form post from another site |
| 404, 405 | An unknown path; a wrong method; an unknown or malformed recording id or speaker profile id |
| 409 | Releasing a recording flagged with a transcription error; a speaker profile name another profile has |
| 412, 416 | A recording's audio fetched with a precondition that fails, or a range past its end or malformed |
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
| Recordings keep form | 1 KiB | fixed |
| Recording review form | 51,776 bytes | fixed |
| Correct text of a recording | 16 KiB | fixed |
| Speaker profiles | 64 | fixed |
| Speaker profile name, after trimming spaces | 64 bytes | fixed |
| Speaker profile form | 1 KiB | fixed |
| Recordings on one page | 100 | fixed |
| Slowest fetch of a recording's audio | 16 KiB/s | fixed |
| Recordings waiting to be saved: audio, transcript and 512 bytes each | 64 MiB | fixed |
| Transcript shown on the recordings page | 2 KiB a recording | fixed |
| Recordings kept when not marked | 7 days | fixed |
| All recordings together | 1 GiB | fixed |

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

With `recordings.enable` on, the service then saves the recordings still
waiting, until `SHUTDOWN_TOTAL` plus `SHUTDOWN_HARD` after step 2 began. It
drops any left at that time with one warning that gives their count, and
does not wait for a save that is still writing.

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
