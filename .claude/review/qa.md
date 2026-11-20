# QA

Judge tests by what they would catch. For every changed behaviour ask which
test fails if it is wrong. The failure scenario of a test finding is the
mutation to the production code the suite would not catch and the test
command that stays green; never claim the mutation exists in production.
The costliest gap here is audio that is silently wrong: a reply that plays
but has the wrong length, gain or timing, or an upload decoded to the wrong
frames.

## Conventions

- testify `require` and/or `assert` in every test file; test names read
  `TestUnit_Condition_Result` (`TestTTSGate_Abandon_HandedSlotPassesOn`).
- HTTP tests drive the real handlers through `newHarness` and
  `newBackendHarness` in `internal/openai/harness_test.go`, against
  `fakeUpstream`; assert what upstream received with `lastCall`, `callsTo`
  and `callCount`, and the client's error text with `errorMessage`.
  Pipeline tests build a `Pipeline` with `newPipeline` in
  `internal/pipeline/fake_test.go` and assert the error's kind and message
  with `requireKind`.
  `speech_encoding_test.go` swaps the package variable `newEncoder` to
  force encoder failures.
- Oracles: `requireTool` in `internal/audio/oracle_test.go` and the helper
  in `internal/openai/speech_encoding_test.go` skip when `opusinfo`,
  `opusdec` or `ffmpeg` is missing. A new encoder assertion that only an
  oracle checks passes silently without those tools; say so in the
  Verification hint.
- Fixtures: `internal/audio/testdata` clips with the commands that made
  them in `testdata/README.md`; `audio_test.go` pins `fixtureFrames`.
  Replacing a fixture needs confirmation, so a changed fixture without a
  README update and a stated reason is a finding.
- `opusCreated` and `opusDestroyed` (`libopus.go`) let tests prove every C
  encoder is freed; `encoder_test.go` shows the pattern.
- Fuzz targets `FuzzNormaliseUpload`, `FuzzOpusPacket` (`fuzz_test.go`),
  `FuzzStretcher` (`wsola_test.go`) and `FuzzReadEvent`
  (`internal/wyoming/fuzz_test.go`). Deleting a seed needs confirmation.
- A new input gets a named limit and a test at the limit, per AGENTS.md;
  `TestNormaliseUpload_OpusAtLimit_Passes` next to `..._OpusOverLimit_TooLong`
  is the shape.
- `tools/test.txt` is `go test ./...`; `tools/race.txt` is
  `go test -race ./...`, every package, `internal/audio` included.

## Lenses, in priority order

1. Silent wrong audio: frame counts, granule and pre-skip, gain and
   clipping, WSOLA length, resampled length. Assert exact values, or decode
   with an oracle, rather than "non-empty".
2. Limits and rejections: each bound tested at the limit and one past it;
   each rejected format or field has a test beside the accepted ones.
3. Status and error text: the client sees the status and message
   `docs/api.md` promises, including 503 busy, 504 on timeout and the 400
   wording for undecodable audio.
4. Concurrency: TTS queue order, slot release on every path
   (`TestServer_Limited_StalledBodyReleasesSlot` is the shape), shutdown
   steps in `cmd/llama-swap-voice/main_test.go`.
5. Flakiness: existing tests synchronise with `time.Sleep` in places; a new
   test that does so where a channel, a fake clock or `testing/synctest`
   would work is a finding.

Weak assertions: `NoError` alone, `Error` without `ErrorIs` on the
sentinel, a length check without the values, a 4xx check without the body.

## Check for what is absent

- Every error branch the change adds is reached by a test that asserts the
  status and message.
- Every changed parser has a fuzz target reaching it.
- A fix lands with a test that fails on the tree before it.

## Finding fields

- Production code: path:start-end that should be tested
- Test code: path:start-end of the weak test, or ABSENT
- Would catch: the defect that slips through without this test
