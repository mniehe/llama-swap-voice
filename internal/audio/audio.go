// Package audio turns an untrusted upload into the 16 kHz mono 16-bit WAV the
// speech recogniser expects, without spawning any external decoder, and
// encodes streamed 24 kHz speech into the reply formats.
package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"time"
)

var (
	ErrUnsupported = errors.New("unsupported audio format")
	ErrCorrupt     = errors.New("corrupted audio")
	ErrTooLong     = errors.New("audio exceeds the duration limit")
)

type Limits struct {
	MaxDuration time.Duration
}

type decodeFunc func(data []byte, out *normaliser) error

// NormaliseUpload decodes WAV, Ogg Opus or MP3 into a 16 kHz mono
// 16-bit WAV, stopping as soon as the decoded audio passes the duration limit.
func NormaliseUpload(ctx context.Context, r io.Reader, limits Limits) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}

	decode, err := sniff(data)
	if err != nil {
		return nil, err
	}

	out := newNormaliser(ctx, limits)
	if err := decode(data, out); err != nil {
		return nil, err
	}

	return out.finish()
}

// The third-party decoders were written for trusted files, so a malformed
// upload that panics inside one is reported as corrupt rather than crashing
// the request. Panics in this package's own code are left to propagate.
func guarded(call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &decoderPanic{value: recovered, stack: debug.Stack()}
		}
	}()

	return call()
}

type decoderPanic struct {
	value any
	stack []byte
}

func (p *decoderPanic) Error() string {
	return fmt.Sprintf("%v: decoder panicked: %v", ErrCorrupt, p.value)
}

func (p *decoderPanic) Unwrap() error {
	return ErrCorrupt
}

// Stack lets a caller log where the decoder crashed without this package
// exporting the type.
func (p *decoderPanic) Stack() []byte {
	return p.stack
}

func corrupt(err error) error {
	if errors.Is(err, ErrCorrupt) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrCorrupt, err)
}
