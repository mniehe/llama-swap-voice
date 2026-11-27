// Package pipeline holds the limits and audio steps that every entrypoint
// shares: admission, deadlines, the TTS queue, upload decoding, upstream
// calls and time-stretch. Entrypoints translate its typed errors to their
// own protocol.
package pipeline

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const maxUploadBytes = 25 << 20

var errBusy = &Error{Kind: KindBusy, Message: "too many concurrent requests, retry shortly"}

type Config struct {
	TTSModel         string
	ASRModel         string
	Voices           map[string]string
	DefaultVoice     string
	Lang             string
	RequestTimeout   time.Duration
	MaxInputChars    int
	MaxConcurrent    int
	MaxConcurrentTTS int
	TTSConcurrency   int
	ASRPrefixRegex   string
	// Recorder keeps the audio of each successful transcription; nil keeps
	// nothing.
	Recorder Recorder
}

// Recorder keeps wav, the 16 kHz mono WAV sent upstream, with the source
// entrypoint, the transcript returned to the client and the request's id for
// its log lines. Record must not wait on the disk, and takes ownership of wav.
type Recorder interface {
	Record(wav []byte, source, transcript, requestID string)
}

type Pipeline struct {
	cfg       Config
	upstream  *upstream.Client
	prefix    *regexp.Regexp
	voices    []string
	slots     chan struct{}
	ttsSlots  chan struct{}
	ttsGate   *ttsGate
	admission admission
	log       *slog.Logger
}

func New(cfg *Config, client *upstream.Client, log *slog.Logger) (*Pipeline, error) {
	prefix, err := compilePrefix(cfg.ASRPrefixRegex)
	if err != nil {
		return nil, err
	}

	return &Pipeline{
		cfg:      *cfg,
		upstream: client,
		prefix:   prefix,
		voices:   voiceNames(cfg.Voices),
		slots:    make(chan struct{}, cfg.MaxConcurrent),
		ttsSlots: make(chan struct{}, cfg.MaxConcurrentTTS),
		ttsGate:  newTTSGate(cfg.TTSConcurrency),
		log:      log,
	}, nil
}

// Work names the slot pool a request draws from.
type Work int

const (
	WorkTranscribe Work = iota
	WorkSpeak
)

type Limits struct {
	RequestTimeout time.Duration
	MaxInputChars  int
	MaxUploadBytes int64
}

func (p *Pipeline) Limits() Limits {
	return Limits{
		RequestTimeout: p.cfg.RequestTimeout,
		MaxInputChars:  p.cfg.MaxInputChars,
		MaxUploadBytes: maxUploadBytes,
	}
}

type Catalogue struct {
	ASRModel     string
	TTSModel     string
	Voices       []string
	DefaultVoice string
	Lang         string
}

func (p *Pipeline) Catalogue() Catalogue {
	return Catalogue{
		ASRModel:     p.cfg.ASRModel,
		TTSModel:     p.cfg.TTSModel,
		Voices:       slices.Clone(p.voices),
		DefaultVoice: p.cfg.DefaultVoice,
		Lang:         p.cfg.Lang,
	}
}

func (p *Pipeline) RequestDeadline(now time.Time) time.Time {
	return now.Add(p.cfg.RequestTimeout)
}

// Admit takes a slot without waiting. The deadline is the caller's, so an
// entrypoint can bound its connection before it learns whether a slot is
// free. The release function cancels the returned context and frees the slot.
func (p *Pipeline) Admit(ctx context.Context, work Work, deadline time.Time) (context.Context, func(), error) {
	slots := p.slots
	if work == WorkSpeak {
		slots = p.ttsSlots
	}

	select {
	case slots <- struct{}{}:
	default:
		return nil, nil, errBusy
	}

	admitted, cancel := context.WithDeadline(ctx, deadline)
	release := sync.OnceFunc(func() {
		cancel()
		<-slots
	})

	return admitted, release, nil
}

type requestIDKey struct{}

// WithRequestID tags ctx so every pipeline log line for the request carries id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestID(ctx context.Context) slog.Attr {
	return slog.String("request_id", requestIDOf(ctx))
}

// requestIDOf returns the id WithRequestID tagged ctx with, or "".
func requestIDOf(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}

	return ""
}

func compilePrefix(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}

	return regexp.Compile(pattern)
}

func voiceNames(voices map[string]string) []string {
	names := make([]string, 0, len(voices))
	for name := range voices {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}
