package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

// Kind says what went wrong in terms an entrypoint can map to its own
// protocol, without knowing which step failed.
type Kind int

// The zero value names no kind, so an Error built without one is answered as
// an internal failure rather than as the client's mistake.
const (
	KindInvalid Kind = iota + 1
	KindBusy
	KindTimeout
	KindUpstream
	KindShuttingDown
	KindInternal
)

const internalErrorText = "internal server error"

// Error carries the public message and enough of the upstream reply for an
// entrypoint to answer and log as it did before the pipeline existed.
type Error struct {
	Kind           Kind
	Message        string
	Upstream       *upstream.StatusError
	UpstreamStatus int
	Retried        bool
	Err            error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Err
}

// ErrShuttingDown is what an entrypoint sends when Enter refuses a request.
var ErrShuttingDown = &Error{Kind: KindShuttingDown, Message: "server is shutting down"}

var audioTooLongText = fmt.Sprintf("audio is longer than %d minutes", maxAudioMinutes)

func invalid(format string, args ...any) *Error {
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf(format, args...)}
}

// wrapped wraps err with its own text as the message.
func wrapped(kind Kind, err error) *Error {
	return &Error{Kind: kind, Message: err.Error(), Err: err}
}

// endedOr blames an ended request before the step that noticed it, since a
// deadline that passed explains whatever the step then reported.
func endedOr(ctx context.Context, kind Kind) Kind {
	if ctx.Err() != nil {
		return KindTimeout
	}

	return kind
}

// upstreamFailure prefers the upstream's own status over an ended request,
// because a status means the upstream answered before the deadline.
func (p *Pipeline) upstreamFailure(ctx context.Context, err error) *Error {
	var statusErr *upstream.StatusError
	if errors.As(err, &statusErr) {
		result := wrapped(KindUpstream, err)
		result.Upstream = statusErr
		result.UpstreamStatus = statusErr.Code
		return result
	}

	if ctx.Err() != nil {
		p.log.Info("request ended before upstream answered", requestID(ctx), slog.Any("error", err))
		return wrapped(KindTimeout, err)
	}

	return wrapped(KindUpstream, err)
}

// Hermes retries a 400 through its own transcoder only when the message says
// "unsupported", "corrupted" or "invalid file", so those words must survive.
// A deadline that passed mid-decode explains any decoder error, so it wins.
func (p *Pipeline) decodeFailure(ctx context.Context, err error) *Error {
	switch {
	case ctx.Err() != nil:
		return wrapped(KindTimeout, fmt.Errorf("decoding audio: %w", ctx.Err()))
	case errors.Is(err, audio.ErrTooLong):
		return &Error{Kind: KindInvalid, Message: audioTooLongText, Err: err}
	case errors.Is(err, audio.ErrUnsupported):
		return p.rejectAudio(ctx, audio.ErrUnsupported, err)
	case errors.Is(err, audio.ErrCorrupt):
		return p.rejectAudio(ctx, audio.ErrCorrupt, err)
	default:
		p.log.Error("could not read upload", requestID(ctx), slog.Any("error", err))
		return &Error{Kind: KindInternal, Message: internalErrorText, Err: err}
	}
}

// The decoder's detail names offsets and library internals, so it goes to
// the log and the client gets only the reason.
func (p *Pipeline) rejectAudio(ctx context.Context, reason, err error) *Error {
	p.logDecodeError(ctx, err)
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf("could not decode audio: %v", reason), Err: err}
}

func (p *Pipeline) logDecodeError(ctx context.Context, err error) {
	var crash interface{ Stack() []byte }
	if errors.As(err, &crash) {
		p.log.Error("audio decoder panicked", requestID(ctx), slog.Any("error", err), slog.String("stack", string(crash.Stack())))
		return
	}

	p.log.Warn("could not decode audio", requestID(ctx), slog.Any("error", err))
}
