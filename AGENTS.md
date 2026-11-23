# Project Context

llama-swap-voice is a Go HTTP service that puts OpenAI's audio API
(`/v1/audio/speech`, `/v1/audio/transcriptions`) in front of llama-swap. When
enabled, it also serves the Wyoming protocol on a second TCP port, for Home
Assistant, through the same pipeline. It decodes uploads, encodes replies
(mp3, opus, wav, pcm) and changes speech speed in-process; it runs no
external programs. `README.md` and the three pages under `docs/` are the
user-facing contract. `README.md` holds the quick start, voices, the common
options and the licences. `docs/options.md` holds every module option and
every environment variable. `docs/api.md` holds the endpoints, status codes,
formats, limits and shutdown of both the HTTP API and the Wyoming listener.
`docs/qwen-tts-server.md` holds the Python server's configuration keys,
endpoints and errors.

`python/` holds a second program, `qwen-tts-server`: the Python
faster-qwen3-tts server that the `qwentts` backend calls on a GPU. Its
launcher, not the Go service, puts `sox` on `PATH`.

Your closing summary names every file you changed and every behaviour change,
including ones nobody asked for, because reviewers read the summary before the
diff.

## Working

Finishing work applies when a person reads this session directly. An agent
working from a brief follows its role prompt there instead: it stops the way
that prompt says and returns its packet with nothing around it. What needs
confirmation applies to every agent.

### Finishing work
Please avoid stopping while you still owe work the user asked for. Be persistent and do not stop unless needed.

While status notes are welcome, as is clarifying with the user on open decisions, carry on with work that does not depend on the user's answer without unnecessary stops.

If you notice yourself inviting the user to redirect you or offering to wait, instead proceed on the next part of the task. You may meet ordinary obstacles like errors, timeouts, locked files, empty results or failing tools. Diagnose them first, and when they are not real blockers, work through them with the access you have (wait and retry, fix the request, use another tool or source) rather than stopping unnecessarily.

If someone clearly decided something is a hard blocker, such as a file marked not to be touched, access that was intentionally withheld, or a safety guardrail, leave it alone, say plainly what you found, and look for another safe way to finish the task. Stopping before the task is complete is very rarely merited, as in when the task can't move forward without user input, or where the blocker is deliberate and should not be worked past. This does not override the need for confirmation on risky or destructive actions.

Before you start, say in a line what you're about to do; brief updates while you work help the user follow along. Close with a short recap that stands on its own (what you found, what you did, and what's next), so a reader who only sees the last message has the full picture.

### What needs confirmation

Commit on a branch other than `main`. Ask first, naming the exact action,
before:

- pushing, merging, or anything that lands on `main`;
- adding, upgrading or replacing a dependency in `go.mod`. A binary carries
  each dependency's licence (`README.md` § Licence), and the audio decoders
  parse untrusted uploads. `gomod2nix.toml` is regenerated in the same change:
  `nix develop -c gomod2nix generate`. Updating `flake.lock` (`nix flake
  update`) needs the same confirmation, because it changes the libopus the
  binary links and the Go toolchain;
- adding, upgrading or replacing a dependency in `python/pyproject.toml`, or
  re-locking `python/uv.lock`. The server's environment carries each
  dependency's licence (`README.md` § Licence), and a CUDA wheel change can
  build, import and pass every check here yet fail only on the GPU;
- adding a call that runs an external program from non-test code, since the
  service was moved off ffmpeg on purpose;
- loosening `.golangci.yaml`, deleting tests or fuzz seeds, or skipping a
  Verification command whose condition applies to the change;
- replacing a file under `internal/audio/testdata/`. Tests pin exact frame
  counts for the WAV and Opus clips, a bounded padding for the MP3 and a
  rejection for the M4A, and `testdata/README.md` records the command that
  made each one.

Routine and needs no confirmation: every command under Verification, entering
the dev shell, running the service against a local llama-swap, adding tests
and fixtures, and editing any other file inside the worktree. An action in
the list above needs confirmation even when it is only a file edit.

## Structure

```
/cmd/llama-swap-voice   # Entry point: config.go (service config), main.go (wiring, signals, three-step shutdown)
/internal
  /audio          # Decode untrusted uploads to 16 kHz mono WAV; encode replies (wav, pcm, mp3, Ogg Opus via cgo libopus); resample; WSOLA time-stretch
    /testdata     # Reference clips and the commands that produced them
  /config         # Generic koanf loading from LLAMA_SWAP_VOICE_* environment variables
  /observability  # slog logger construction (json or console)
  /openai         # OpenAI HTTP entrypoint: handlers, request bounds, response encoding, panic recovery, request log, JSON errors
  /pipeline       # Transport-neutral steps every entrypoint calls: admission pools, request bounds (input characters, upload size, decoded duration), request deadline, the TTS queue, shutdown admission, upload decoding, upstream calls, time-stretch, typed errors
  /wyoming        # Wyoming TCP entrypoint: bounded event codec, connection state machine, deadlines, describe, ping, transcription, single-event and streamed synthesis, the sentence splitter, Shutdown
  /upstream       # llama-swap client: TTS backends (llamacpp, qwentts), transcription, one retry, no redirects
  /voices         # Finds and checks voice clips at startup; the llamacpp backend reopens the clip on each request
/nix                    # packages.nix (the build), checks.nix (what nix flake check runs), devshells.nix (nix develop), module.nix (nixosModules.default), llama-swap.nix (the module's llama-swap entries and unit changes), models.nix (pinned GGUFs), qwen-tts-config.nix (the TTS server's generated config)
  /tests                # NixOS module checks: default.nix (lists them), eval.nix (assertions, generated environment, llama-swap settings), vm.nix (VM test against fake-upstream.py, a strict fake TTS and ASR upstream), llama-swap-vm.nix (VM test under nixpkgs' llama-swap with the fakes in fakes.nix and fake-model-server.py, and a Wyoming round trip), wyoming-vm.nix (Wyoming from a client VM: the address allow list and the firewall, one control at a time), wyoming-client.py (stdlib Wyoming client both VM tests drive), qwen-tts-config.nix (config.py loads the generated config), model-pins.nix (default GGUF pins, evaluation only), duration-corpus.nix with durations.nix and testdata/parse-duration.go (durations compared with Go)
  /qwen-tts-server      # The TTS server's CUDA environment (uv2nix), wheel overrides, weights pin, tests and launcher
/python                 # qwen-tts-server: the faster-qwen3-tts HTTP server the qwentts backend calls; pyproject.toml and uv.lock pin its environment
  /qwen_tts_server      # app.py (endpoints), config.py, engine.py (the model), worker.py (the one CUDA thread), text.py, wav.py
  /tests                # GPU-free tests with a fake engine; test_tokenizer.py alone needs torch
/docs                   # Reference pages of the user-facing contract: options.md (module options, environment variables), api.md (endpoints, status codes, limits), qwen-tts-server.md (the Python server)
flake.nix               # Package, dev shell and checks; flake.lock pins the inputs
gomod2nix.toml          # Hash of every Go module for the Nix build; regenerated with go.mod
.version                # Version stamped into the binary; cocogitto (.cog.toml) writes it on a bump
```

## Development

The build needs cgo, a C compiler, pkg-config and libopus. The flake's dev
shell provides them, so every Go command runs inside it:

```bash
nix develop                    # an interactive shell
nix develop -c go test ./...   # one command
```

`opus-tools` and `ffmpeg` are test oracles: tests decode the service's own
output with them and skip those checks when the tools are missing. Run with
them present, or a broken encoder passes.

The Go tests and the GPU-free Python suite need no GPU, model or running
llama-swap; the Python tokenizer test, which needs torch, is the one exception.
HTTP tests drive the real handlers against the fake upstream in
`internal/openai/harness_test.go`; pipeline tests call `internal/pipeline`
against their own fake upstream in `internal/pipeline/fake_test.go`; Wyoming
tests drive a real loopback listener with a fake client and their own fake
upstream in `internal/wyoming/harness_test.go`.

The Python server under `python/` is built and tested through the flake, on
x86_64 Linux only. Its tests run without torch against a fake engine; the
CUDA environment and the weights build only when asked for:

```bash
nix build -L .#checks.x86_64-linux.qwen-tts-server-tests   # GPU-free tests, no torch
nix build -L .#qwen-tts-server.tokenizer-test              # the one test that needs torch; builds the CUDA environment
nix build .#qwen-tts-server.env                            # the CUDA environment and its install check
```

After a confirmed change to `python/pyproject.toml`, re-lock in `python/` with this
flake's nixpkgs, `nix shell --inputs-from .. nixpkgs#uv nixpkgs#python312 -c uv
lock`, and compare the resolved versions before and after.

## Rules

`.golangci.yaml` is the source of truth; do not loosen it to make a change
pass. Its choices that change how code is written:

- `gocritic` enables every check, `hugeParam` and `rangeValCopy` included, so large structs travel by pointer.
- `gosec` G115 is on, so a narrowing conversion is bounded first and says so.
- `govet` `shadow` is on, so inner `err` declarations get distinct names.
- `errcheck` checks blank assignments and type assertions, so `_ = fallible()` and a bare `x.(T)` fail lint.
- `misspell` uses the UK locale.

A suppression names the linter and states why.

- Return errors wrapped with `%w`. `os.Exit` belongs to `main` alone, and the
  only panics outside tests are the `http.ErrAbortHandler` re-panics in
  `internal/openai`, which abort a response that has already started.
- Everything a client or the upstream sends is read through a bound: body
  size, decoded duration, transcript size, input characters. A new input
  gets a named limit and a test at the limit.
- A third-party call that parses untrusted compressed audio runs under
  `guarded` in `internal/audio/audio.go`, so a decoder panic becomes a 400
  and not a crashed request. Decoder setup from validated values and plain
  accessors run outside it.
- Replies stream. Encode and write as audio arrives; do not buffer a whole
  reply to simplify a code path.
- HTTP error responses are JSON `{"error":{"message":"…"}}`, written with
  `writeError` in `internal/openai/respond.go`. Two exceptions: the readiness
  probe answers a plain-text 503 while shutting down, and a failure after the
  response is committed aborts the transfer, because JSON appended to audio
  corrupts it. Writing the Opus headers commits the response before the
  encoder constructor returns. A Wyoming failure is an `error` event with
  `text` and `code`, sent before the connection closes.
- Packages log through the `*slog.Logger` they were constructed with. Only
  `main` touches the default logger.
- A change to an endpoint, status code, limit or environment variable updates
  the page that documents it in the same change: `docs/api.md` for an
  endpoint, status code or limit, `docs/options.md` for an environment
  variable or module option, `docs/qwen-tts-server.md` for the Python server,
  and `README.md` where it repeats the value. The change also updates
  `nix/module.nix` with `nix/tests/eval.nix` when the variable has a module
  option, because the module names each variable by hand and no test compares
  it with `config.go`.
- Values from a spec or shared across call sites are named constants.

The Python server under `python/` follows three of these rules too: every
client input is read through a named bound with a test at the limit, every
error before audio is the JSON envelope `{"error":{"message":"…"}}`, and
values from a spec or shared across call sites are named constants.

## Verification

A task is complete only when the commands below have run in this session,
inside the dev shell, and their output shows success. Report the commands and
results in the closing summary; "should pass" is not a result. There is no
hosted CI and no push hook: run `nix flake check` yourself before every push.
`cog bump` also runs it.

```bash
gofmt -l . && go vet ./... && golangci-lint run ./...   # gofmt -l must print nothing
go test ./...
go test -race -timeout 30m ./...     # when the change touches goroutines, channels, locks, internal/openai, internal/pipeline or internal/wyoming
go test -run '^$' -fuzz '^FuzzNormaliseUpload$' -fuzztime 30s ./internal/audio   # when the change touches a Go parser or the time-stretch
go test -run '^$' -fuzz '^FuzzReadEvent$' -fuzztime 30s ./internal/wyoming   # when the change touches the Wyoming event codec
go test -run '^$' -fuzz '^FuzzSplitter$' -fuzztime 30s ./internal/wyoming    # when the change touches the Wyoming sentence splitter
nix flake check                      # before a push: race tests, lint, gofmt, alejandra, Opus conformance, package version, gomod2nix sync, NixOS module eval, VM tests (with and without llama-swap, the Wyoming round trip, the Wyoming address test), TTS server config and GGUF pins, duration corpus, the Python server's tests and weights pin
nix build -L .#checks.x86_64-linux.qwen-tts-server-tests   # when the change touches python/ or nix/qwen-tts-server/
nix build -L --no-link .#checks.x86_64-linux.module-eval .#checks.x86_64-linux.duration-corpus .#checks.x86_64-linux.module-model-pins .#checks.x86_64-linux.module-qwen-tts-config .#checks.x86_64-linux.module-vm .#checks.x86_64-linux.module-llama-swap-vm .#checks.x86_64-linux.module-wyoming-vm   # when the change touches nix/module.nix, nix/llama-swap.nix, nix/models.nix, nix/qwen-tts-config.nix, nix/tests/ or python/qwen_tts_server/config.py
nix build .#qwen-tts-server.env .#qwen-tts-server.tokenizer-test   # when the change touches python/pyproject.toml, python/uv.lock, nix/qwen-tts-server/overrides.nix, python/tests/test_tokenizer.py or python/qwen_tts_server/engine.py
```

Run the fuzz target that reaches the changed code: `FuzzNormaliseUpload` for
upload decoding, `FuzzOpusPacket` for Opus packets, `FuzzStretcher` for WSOLA,
`FuzzReadEvent` (in `./internal/wyoming`) for the Wyoming event reader,
`FuzzSplitter` (in `./internal/wyoming`) for the sentence splitter.
A passing target that does not reach the change is not coverage. No target
reaches `NewWAVReader` in `wavreader.go`; a change to its header parsing adds
one.

Also exercise the changed path once for real when a llama-swap is reachable:
run the service and send the `curl` requests from `README.md` § Quick start.
Say so in the summary when none was reachable.

Review every change to code or prompts before it is done and resolve every
Must Fix. What changed sets the depth, unless a Researcher brief's REVIEW line
says otherwise:

| Change | Review |
|---|---|
| Only `README.md`, `docs/**`, `LICENSE` or other documentation | None |
| Only prompt files, none of them a critical path | `/pantheon:review --dimension prompt`: one reviewer |
| Under 200 source lines, no critical path | `/pantheon:review --dimension correctness`: one reviewer, enough for a diff one agent can hold |
| 200 to 1000 source lines, or a critical path at any size | `/pantheon:review`: the manifest selects the dimensions |
| Over 1000 source lines | Split into ranges of at most 1000 and review each; one review cannot cover more |

Take the line count from the source-lines figure, S, on the
`Files: N files, M changed lines, S source lines` line that
`pantheon-cli review prep` prints. Prep measures the diff the review covers,
uncommitted work included. A `pantheon-cli` older than the source-lines
change prints no S; use M then, and say so in the summary.

Source lines do not count prompt files. A change that touches a prompt file
together with code gets the review its size calls for and then, unless that
run's `Dimensions:` line already lists `prompt`, a second run of
`/pantheon:review --dimension prompt` on the same range. Do not add the flag
to the first run: `--dimension` replaces the manifest's selection.

Prompt files are what an agent reads as instructions: `AGENTS.md`, `CLAUDE.md`
(a symlink to it), `.claude/review/*.md` and `.claude/review/review.yaml`,
whose `verify_notes` every verifier reads.

Critical paths, where a small mistake is a memory-safety or denial-of-service
hole, or silently corrupts audio:

- `internal/audio/**`: parses untrusted uploads and calls libopus through cgo
- `internal/voices/**`: reads operator files and follows symlinks
- `internal/upstream/**`: retry, redirect refusal and response bounds
- `internal/openai/**`: request bounds, response commitment, panic recovery
- `internal/wyoming/**`: parses untrusted frames, connection bounds (count, idle, read and write deadlines), request cleanup, synthesis and streams with their TTS slots
- `internal/pipeline/**`: the admission pools, the request bounds (input characters, upload size, decoded duration), the TTS queue, the request deadline, shutdown admission
- `cmd/llama-swap-voice/**`: limit validation, connection deadlines, the three-step shutdown
- `go.mod`, `go.sum`, `.golangci.yaml` and `.claude/review/review.yaml`, which holds shell commands that review prep runs
- `flake.nix`, `flake.lock`, `gomod2nix.toml` and `nix/**`: the toolchain, the build and the checks that gate a push
- `.cog.toml`: it holds shell commands that `cog bump` runs
- `python/qwen_tts_server/**`: request bounds and JSON parsing, the single CUDA worker and its queue, cancellation, and an exit on fatal CUDA errors
- `nix/qwen-tts-server/**`, `python/pyproject.toml`, `python/uv.lock`: the CUDA wheels' RUNPATHs and the weights' hashes; a missing `$ORIGIN` builds and imports yet fails at CUDA-graph warm-up on the GPU

### Definition of Done

A change is done when everything this Verification section requires of it has
run and passed, review included. Four rules apply on top of that:

- `nix flake check` passes before a push, on Linux with KVM, because the flake defines no checks on Darwin and the VM checks boot virtual machines.
- A behaviour change carries a test that failed before the change and passes after it, so the test is known to detect the fault.
- Each commit is SSH-signed with a single-line conventional subject, `type(scope): subject`, because the history is the changelog cocogitto reads.
- No commit, comment, code or document carries AI attribution, because the owner publishes the work as his own.
