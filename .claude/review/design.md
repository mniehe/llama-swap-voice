# Design

Challenge every abstraction, dependency and boundary the change adds or
moves. This is a small Go service of flat packages with concrete types; the
common failure is recommending a textbook pattern that adds a layer without
a bug it prevents or a test it enables.

## Shape of the service

- Imports run one way: `cmd/llama-swap-voice` wires everything;
  `internal/openai` imports `internal/audio` and `internal/upstream`;
  `audio`, `upstream` and `voices` import no other internal package.
  A reverse or sideways import is a finding.
- `internal/config` is generic koanf loading (`LoadConfig[T]`); the
  service's own settings, defaults and `Validate` live in
  `cmd/llama-swap-voice/config.go`, and `serverConfig` maps them onto
  `server.Config`.
- `internal/audio` exports a small surface: `NormaliseUpload`, `Limits`,
  `NewEncoder`, `Format`, `NewWAVReader`, `NewStretcher`, `MinSpeed`,
  `MaxSpeed` and three sentinel errors. `decoderPanic` stays unexported
  and reaches `internal/openai` through a `Stack()` interface check; that
  is the pattern for extra detail across the boundary.
- cgo is confined to `internal/audio/libopus.go`. Another file importing
  "C", or a C type escaping the `libopus` struct, is a finding.
- The service runs no external programs; non-test code that does needs
  confirmation (AGENTS.md).
- Packages log through the `*slog.Logger` they were constructed with; only
  `main` touches the default logger.
- Closed sets are string types with constants (`audio.Format`,
  `upstream.Backend` with `ParseBackend`); options are functional
  (`upstream.Option`, `WithBackend`).
- Tests reach seams through real HTTP (`fakeUpstream`), one package
  variable (`newEncoder`) and standard library interfaces
  (`http.RoundTripper` in `roundTripFunc`, `io.WriteCloser` in
  `panickingEncoder`); no interface exists only for tests.

## Lenses, in priority order

1. Design that causes or hides a bug: an invariant enforced in two places
   that can drift (a limit in both `server` and `audio`), a resource whose
   owner is unclear across a package boundary (the TTS slot, the Opus
   encoder handle, the upstream body).
2. Streaming broken by structure: an API that takes `[]byte` where a
   reader or writer keeps replies streaming.
3. Layering: inbound HTTP belongs in `server`, outbound llama-swap HTTP in
   `upstream`, codecs and DSP in `audio`, generic loading in
   `internal/config`, service defaults and validation in `cmd`. HTTP in
   `audio`, codec detail in `server` handlers, or service settings parsed
   outside `cmd` is a finding.
4. Surface: a new export with no caller outside its package, visibility
   widened without need, a boolean parameter where a named type reads at
   the call site.
5. Premature abstraction: an interface with one implementation added for
   mocking when `fakeUpstream` or a package variable already serves.
6. Naming and organisation.

## Check for what is absent

- A sentinel error exported when a caller in another package needs
  `errors.Is`, as `audio.ErrCorrupt` is for `decodeFailure`.
- A new configuration value threaded through `ServiceConfig`, its owning
  component and `docs/options.md` together; only server settings go through
  `serverConfig` (`main` reads host, port, logging and shutdown itself).
- A named constant for a value from a spec or shared between call sites.

## Finding fields

- Principle: the design principle at stake
- Impact: how this harms correctness, testability or change
- Trade-offs: what the recommended change costs
