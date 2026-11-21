# Security

Find risks the change introduces or widens that an attacker could use.
Every finding states the actor or the access they need, the trust boundary
crossed, the asset, and what they gain. The service has no authentication:
any client that reaches the port can call every route, so the questions are
memory safety, resource exhaustion and disclosure. AGENTS.md names
`internal/audio`, `internal/voices`, `internal/upstream`, `internal/openai`,
`internal/wyoming`, `internal/pipeline`, `cmd/llama-swap-voice` and the build
files critical paths, where a small
mistake is a memory-safety or denial-of-service hole.

## Trust boundaries

- Clients, through `internal/openai`: `POST /v1/audio/speech` and
  `/v1/audio/transcriptions`, also served without the `/v1` prefix
  (`routePrefixes`). Speech JSON is bounded by `maxSpeechBodyBytes` and
  `MaxInputChars` (runes, `pipeline.PrepareSpeech`); the voice comes from
  the `cfg.Voices` map, never a client path; `speed` is held to
  `audio.MinSpeed`..`audio.MaxSpeed` (`pipeline.ResolveSpeed`). Uploads are
  bounded by `Limits().MaxUploadBytes` through `http.MaxBytesReader`, and
  `ParseMultipartForm(uploadMemory)` spools the rest to disk.
- Wyoming clients over TCP, through `internal/wyoming`: `ReadEvent` bounds
  the header line (`maxHeaderBytes`), data (`maxDataBytes`) and payload
  (`maxPayloadBytes`) before allocating; `maxConns` caps connections at
  accept. `Serve` retries only `retriedAcceptErrors`, after a capped backoff
  that `Close` and `Shutdown` end, and `Shutdown` counts each `Serve` loop
  in `goroutines`. With no request open, a connection must start one by
  `idleSince` plus `idleTimeout`, checked again for buffered events; describe
  and ping do not extend it. With a request open, each event gets
  `idleTimeout`, clamped to the request deadline. Describe and ping replies
  get `writeTimeout`, cut to the idle deadline when no request is open; the
  event that closes a request or the connection (the transcript,
  `audio-stop` or an `error`) gets the whole `writeTimeout`, and an `error`
  closes the connection. `audio-start` takes an ASR slot, and the recording
  is bounded by `Limits().MaxUploadBytes` minus `wavHeaderBytes` before
  `Transcribe` reads it whole. `synthesize` takes a TTS slot; its `text` and
  `voice.name` go through `pipeline.PrepareSpeech` (`MaxInputChars`,
  `cfg.Voices`). Every reply event (`audio-start`, each `audio-chunk`,
  `audio-stop`) gets the whole `writeTimeout` through `reply`; `sendChunk`
  checks the request context before each chunk, and `closeSpeech` closes the
  PCM, which frees the TTS gate.
- Untrusted audio, `internal/audio`: `NormaliseUpload` reads the whole
  upload, `sniff` picks a decoder: the in-repo WAV parser (`wav.go`), the Ogg
  reader (`ogg.go`, `maxOggPacketBytes`) feeding pion/opus, or go-mp3.
  `normaliser.charge` bills frames before trimming and `write` bills kept
  frames against `asrLimits` (20 minutes); `setRate` bounds the sample
  rate, `maxChannels` the channels.
- cgo, `internal/audio/libopus.go`: encoder only. `encode` copies a frame
  into `pcm` (sized `opusFrameSamples`) and passes C lengths that must match
  the Go buffers they point into; `packet` is `opusMaxPacketBytes`.
- Upstream llama-swap, semi-trusted, `internal/upstream/upstream.go`: error
  bodies read through `maxErrorBodyBytes`, transcripts through
  `maxTranscriptBytes`; reply WAV headers through `NewWAVReader`
  (`maxFormatChunkBytes`, `maxSkippedChunkBytes`); `refuseRedirect` stops a
  POST carrying the voice clip from being replayed elsewhere.
- Operator files, `internal/voices/voices.go`: `Discover` follows symlinks by
  design, keeps regular files only and checks `maxVoiceBytes` at startup;
  `attachFile` in `upstream.go` reads the clip again per request.
- Connection deadlines: `bounded` (`controlRequestTimeout`), `limited`, and
  `readHeaderTimeout` in `cmd/llama-swap-voice/main.go`.
- Build: `go.mod` and `go.sum` (a dependency change needs confirmation; the
  decoders parse untrusted input); `.golangci.yaml` (gosec G115 and the
  errcheck settings are controls); `.claude/review/review.yaml`, whose
  `tools` are shell commands review prep runs.

## Lenses, in priority order

1. Memory safety at the cgo boundary: a length or index passed to C that
   can exceed the Go slice behind the pointer; a C object used after
   `destroy`.
2. Resource exhaustion: an unbounded read from a client or upstream; an
   allocation sized from a header field before it is checked; decoding work
   not billed to `charge`; CPU per input byte that grows with a field the
   client sets; a deadline removed or extended.
3. Parser robustness: a third-party call that parses untrusted compressed
   data outside `guarded` (setup from validated values and plain accessors
   need not be inside); an in-repo parser that can index out of range on
   hostile input.
4. Disclosure: decoder detail, operator paths (voice files) or upstream
   internals in a client-facing message; `rejectAudio` shows the intended
   split between log and client.
5. Outbound requests: a URL part taken from the client instead of config;
   a client built without `withoutRedirects`; a retry beyond `retryable`.
6. Dependencies and lint: a new or upgraded module with a published
   advisory; a gosec or errcheck setting loosened.

## Check for what is absent

- Every new client or upstream input has a named limit and a test at it.
- Every changed parser is reached by a fuzz target that runs the changed
  code: `FuzzNormaliseUpload` (upload decoding), `FuzzOpusPacket` (Opus
  packets), `FuzzStretcher` (WSOLA) or `FuzzReadEvent` (Wyoming frames,
  `internal/wyoming`). None reaches `NewWAVReader`; a change
  to its header parsing without a new target is a finding.
- Every new outbound client keeps `refuseRedirect` and a context deadline.

## Finding fields

- Actor: who supplies the input (client, upstream, operator) and through
  which route or file
- Class: the vulnerability class, when one applies
- Advisory: the advisory id, when one applies
