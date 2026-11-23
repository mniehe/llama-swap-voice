# Correctness

Find changes that are wrong: logic errors, races, broken invariants of a
streaming audio proxy, and discipline violations AGENTS.md forbids. The bugs
worth finding sit between a handler, its middleware and the upstream body it
streams, so trace across files rather than reading hunks alone.

## Project shape

- `cmd/llama-swap-voice/main.go` wires `voices.Discover`, `upstream.New`,
  `pipeline.New` and `openai.New`, then runs `serve` and `shutdown`;
  `config.go` holds `ServiceConfig`, `Validate` and `serverConfig`, which
  builds the `pipeline.Config`.
- `internal/openai/server.go` `Handler` composes
  `logged(recovered(bounded(admitted(mux))))`. `limited` wraps the two audio
  routes: it takes `pipeline.RequestDeadline`, sets the connection deadlines
  (write deadline `responseWriteSlack` later so a 504 still reaches the
  client) before it calls `pipeline.Admit`, which takes a non-blocking slot
  from `ttsSlots` (speech) or `slots` (transcriptions) and applies the
  deadline. `failure` in `failure.go` maps each `pipeline.Kind` to a status.
- `internal/pipeline` holds every step that does not depend on HTTP:
  admission, the TTS queue, `PrepareSpeech`, `Speak`, `Transcribe`, and the
  typed `*pipeline.Error` its callers map to their protocol.
- `internal/audio` decodes uploads into a `normaliser` and encodes replies
  through `NewEncoder`; `libopus.go` is the only cgo file.
- `internal/upstream/upstream.go` talks to llama-swap; `Synthesize` retries once.
- `docs/api.md` is the contract for endpoints, status codes and limits, and
  `docs/options.md` for environment variables and module options;
  `README.md` repeats some values.

## Concurrency: open these together

- `internal/pipeline/admission.go`: `enter` calls `active.Add` under the same
  lock `close` sets `closed` with, so no handler starts once `Drain` waits.
  An `Add` outside that lock races `Wait`.
- `internal/pipeline/ttsgate.go`: FIFO. `acquire` takes a free slot only when
  `waiters` is empty; `release` and `abandon` call `handOver`, which closes
  the oldest waiter's channel instead of incrementing `free`. A waiter whose
  context ends after it was handed the slot is gone from `waiters` and must
  pass the slot on. Every acquired slot is released once: `synthesize` in
  `internal/pipeline/speech.go` releases on upstream error, otherwise
  `gatedBody` releases through `once` on EOF or `Close`; `Speak` closes the
  body on every failure, and `handleSpeech` closes `Speech.PCM` on every
  path after success through `closeSpeech`.
- `internal/openai/recovery.go`: `http.ErrAbortHandler` is re-panicked; once
  `committed` (`trackingWriter.wroteHeader`, `requestlog.go`) a recovered
  panic becomes `ErrAbortHandler`, never a JSON body appended to audio.
- `cmd/llama-swap-voice/main.go` shutdown, three steps: `SetShuttingDown`
  and a `Grace` sleep; `drain` (`httpServer.Shutdown` bounded by `Total`);
  on timeout `cancelRequests` cancels the `BaseContext`, `httpServer.Close`,
  then `awaitHandlers` runs `pipeline.Drain` bounded by `Hard`. Request
  contexts descend from that `BaseContext`, not from the signal context.

## Lenses, in priority order

1. Discipline, from AGENTS.md: a panic outside tests other than the
   `http.ErrAbortHandler` re-panics in `failure.go` and `recovery.go`;
   `os.Exit` outside `main`; `slog.Default` or `slog.SetDefault` outside
   `main`; an error wrapped without `%w`; `_ = fallible()`; a `//nolint`
   without the linter name and reason; a spec value or repeated value that
   is not a named constant; non-test code importing `os/exec`.
2. Concurrency: the invariants above; a lock held across a blocking channel
   send or receive, or I/O (`handOver` closing a waiter's channel under the
   lock is non-blocking and correct); a goroutine with no owner that ends it
   (non-test code starts two: `main.go` listen and `admission.close`).
3. Streaming: replies encode and write as audio arrives (`streamEncoded`,
   `flushWriter`); buffering a whole reply is a finding. The line is
   response commitment (`committed`): Opus construction writes the Ogg
   headers and can commit the 200 before `newEncoder` returns. A failure
   before commitment is JSON; after it, `streamFailure` (`encoderFailure`).
4. Audio arithmetic: sample, frame and byte conversions (`pcmSampleBytes`,
   `opusFrameSamples`, `opusGranuleScale`, pre-skip), resampler positions,
   WSOLA `expected` and `trim`. A narrowing conversion is bounded first.
   Tests pin exact WAV and Opus fixture frames, allow `mp3PaddingLimit` for
   MP3, and expect the M4A fixture to be rejected.
5. Release: `libopus.destroy` on every path (`newOpusEncoder` `handedOver`,
   `opusEncoder.Close`); `streamEncoded` closes the encoder after a failed
   write; `removeUploads` after `ParseMultipartForm`; the slot release
   `pipeline.Admit` returns, deferred in `limited`.
6. Contracts: an endpoint, status, limit or environment variable changed
   without the page that documents it (`docs/api.md`, `docs/options.md`,
   `docs/qwen-tts-server.md`); an error body not written through `writeError` in
   `respond.go`, except the readiness probe's plain-text 503 while shutting
   down (`writeText`) and a failure after commitment, which aborts.

## Check for what is absent

- Every successful `ttsGate.acquire` meets exactly one release on every
  path: upstream error, body never read. A failed acquire holds no slot.
- Every new `ServiceConfig` field is validated and reaches the component
  that owns it; only pipeline settings go through `serverConfig`.
- Every new third-party call that parses untrusted compressed data sits
  inside `guarded` in `audio.go`; setup from validated values and plain
  accessors (`opus.NewDecoderWithOutput`, `SampleRate`) need not.
- A value added to `audio.Format`, `upstream.Backend`, `outputFormats` or
  `transcriptFormats` is handled at every site that branches on the set.

## Finding fields

- Lens: Discipline / Concurrency / Streaming / Arithmetic / Release / Contracts
