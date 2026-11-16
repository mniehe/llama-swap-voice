# Failure paths

Find failures the change lets reach a caller looking like something else.
Trace each error from its origin through propagation, wrapping, fallback,
retry, cleanup and rollback to the caller-visible outcome: a return value,
a status or exit code, a printed or logged line, a persisted record, or
nothing. Establish the intended semantics first, from the brief, a spec or
doc comment, a test, or the caller's handling: a value, absence, partial
success, cancellation and failure are different outcomes, and an empty
result the semantics call absence is not a swallowed error. Where none
settles them, absence and partial success are not findings; duplication,
skipped cleanup and unobserved work still are. A path whose semantics or
trace could not be established is not listed under CLEAN; it is a nit
naming the missing evidence. The one-hop read bounds where origins are
looked for, not the reads needed to establish a candidate's outcome or
termination owner.

## Project shape

Go only. Intended outcomes, from the code and `README.md`:

- Before the response is committed (`committed`) a failure is a JSON error
  through `writeError` (`internal/server/respond.go`); the readiness
  probe's plain-text 503 while shutting down is the one exception. Opus
  construction writes headers and can commit the 200, so `encoderFailure`
  checks. After commitment `streamFailure` (`failure.go`) logs and panics
  `http.ErrAbortHandler`: a broken transfer is intended, not swallowed.
- `upstreamFailure` passes a `StatusError` of 400 or more through with
  `Retry-After` (lower codes become 502) before it looks at the context;
  other errors answer 504 if the context ended (as `unlessRequestEnded`
  does), else 502. Bad speech JSON or multipart is a 400 directly.
- `decodeFailure`: an ended context is 504 first; `ErrTooLong` is a 400
  "audio is longer than 20 minutes"; `ErrUnsupported` and `ErrCorrupt` are
  400s via `rejectAudio` whose text keeps "unsupported" or "corrupted",
  which a client retries on; anything else is logged, 500 `errInternal`.
  `guarded` turns a decoder panic into `ErrCorrupt`; `logDecodeError`
  logs its stack.
- `upstream.Synthesize` retries once, only when `retryable` (transport error
  or 502 with the context alive), sending a body built once. `Transcribe`
  does not retry.
- Named best-effort steps: `closeUpstream`, `logWriteFailure`,
  `removeUploads`, `setDeadlines`.
- Termination owners: the `limited` deadline from `RequestTimeout`; the
  upstream `http.Client` built in `cmd/llama-swap-voice/main.go` has no
  `Timeout`, so that deadline is its only bound. `ttsGate.acquire`,
  `normaliser` and `contextReader` observe the context; shutdown is bounded
  by `ShutdownTimingConfig` `Total` and `Hard`.

| Language | Swallows | Legitimate when the trace confirms |
|---|---|---|
| Go | `_ = f()` or `_, _ = f()` on a call that can fail | `_, _ = b.Write(p)` on a `strings.Builder`, `bytes.Buffer` or `hash.Hash`, documented never to return an error |
| Go | `if err != nil { return nil }`, or `return nil, nil`, in a function returning an error | `errors.Is(err, sql.ErrNoRows)` or `fs.ErrNotExist` mapped to absence where the semantics call it absence |
| Go | `log.Printf(..., err)` then continuing | a best-effort step the semantics name, such as telemetry |
| Go | `fmt.Errorf("...: %v", err)` where callers test the cause with `errors.Is` or `errors.As` | a documented opaque boundary returning an approved public error and logging the cause privately; `%v` still carries the cause's text |
| Go | `defer f.Close()` on a file just written, discarding the error Close reports for the write | `defer f.Close()` or `defer rows.Close()` on a read |
| Go | a network or process wait under `context.Background()`, or a `ctx` no caller bounds | a `ctx` traced to `WithTimeout`, `WithDeadline` or lifecycle cancellation, passed to a call that observes it |

## Lenses, in priority order

1. Disguised success: an error collapsed to nil, null, an empty
   collection, a default value or a success status; an error logged and
   execution continued as if the operation had succeeded.
2. Duplication: a retry after an ambiguous effect (a write that may have
   landed before the timeout or the lost response) without idempotence or
   effective deduplication. A proven no-effect failure, such as connection
   refused before the request was sent, may be retried without them.
3. Skipped cleanup: a release, rollback or compensating action present on
   some exit paths and absent on one; a cleanup error that replaces the
   original error.
4. Lost cause: an error rewrapped without its cause, or a catch that
   raises a different, generic error, so the caller cannot tell which
   failure occurred. A documented opaque boundary that maps to an approved
   public error and keeps the cause in private diagnostics is the
   exception; `%v` formats the error's text and is not redaction.
5. Unobserved work: a promise not awaited, a task, goroutine or future
   whose result or failure no one reads, a rejection with no handler.
6. Missing termination: a wait on the network or another process whose
   required termination condition has no effective owner. Establish the
   condition and the owner (a deadline, a cancellation, EOF or lifecycle
   shutdown) before reporting; a context or signal parameter proves
   neither a bound nor that the callee observes it. A service accept loop,
   a desktop event wait or a CLI reading stdin legitimately waits for
   lifecycle cancellation or EOF.

## Check for what is absent

- Every fallible call the change adds reaches its caller as a failure, or
  the semantics name the outcome it becomes.
- Every catch, recover or fallback that returns a value has a stated
  reason that value is correct for that failure.
- Every retry after an ambiguous effect is over an idempotent or
  deduplicated operation, and every retry states how repeated failure
  terminates.
- Every resource acquired and transaction begun on the changed path is
  released or rolled back on its error exits.
- Every promise, task or goroutine the change starts is awaited, joined or
  has its failure handled.
- Every call the change adds that waits on the network or another process
  has a termination owner it observes.
- Every rewrap keeps the cause, unless it sits at a documented opaque
  boundary that keeps the cause in private diagnostics.

## Finding fields

Add to the standard format:

- OBSERVED: the caller-visible result quoted from the line that states it
  (a status string, log line or test assertion), or "traced, not observed"
  with the line the trace ends at when no line states it
