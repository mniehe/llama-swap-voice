package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/observability"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
	"github.com/mniehe/llama-swap-voice/internal/voices"
	"github.com/mniehe/llama-swap-voice/internal/wyoming"
)

var Version = "0.0.0-dev"

const (
	minPort           = 1
	maxPort           = 65535
	minTTSConcurrency = 1
	minStreamingChars = 1
	languageSeparator = ","
)

var (
	upstreamSchemes = []string{"http", "https"}
	logLevels       = []string{observability.LevelDebug, observability.LevelInfo, observability.LevelWarn, observability.LevelError}
	logFormats      = []string{observability.FormatJSON, observability.FormatConsole}
	streamingModes  = []wyoming.Mode{wyoming.ModeSentences, wyoming.ModeWhole}
)

type ServiceConfig struct {
	Host           string               `koanf:"host"`
	Port           int                  `koanf:"port"`
	Upstream       UpstreamConfig       `koanf:"upstream"`
	Voices         VoicesConfig         `koanf:"voices"`
	TTS            TTSConfig            `koanf:"tts"`
	ASR            ASRConfig            `koanf:"asr"`
	Request        RequestConfig        `koanf:"request"`
	Logger         LoggerConfig         `koanf:"log"`
	ShutdownTiming ShutdownTimingConfig `koanf:"shutdown"`
	Wyoming        WyomingConfig        `koanf:"wyoming"`
	Recordings     RecordingsConfig     `koanf:"recordings"`
}

type UpstreamConfig struct {
	URL            string           `koanf:"url"`
	TTSModel       string           `koanf:"ttsmodel"`
	ASRModel       string           `koanf:"asrmodel"`
	TTSBackend     upstream.Backend `koanf:"ttsbackend"`
	TTSConcurrency int              `koanf:"ttsconcurrency"`
}

type VoicesConfig struct {
	Dir     string `koanf:"dir"`
	Default string `koanf:"default"`
}

type TTSConfig struct {
	Lang          string `koanf:"lang"`
	MaxInputChars int    `koanf:"maxinputchars"`
}

type ASRConfig struct {
	PrefixRegex string `koanf:"prefixregex"`
}

type RequestConfig struct {
	Timeout          time.Duration `koanf:"timeout"`
	MaxConcurrent    int           `koanf:"maxconcurrent"`
	MaxConcurrentTTS int           `koanf:"maxconcurrenttts"`
}

type LoggerConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// WyomingConfig is the Wyoming listener's. Host is a bare address, so an
// IPv6 one is written without brackets. Languages is a comma-separated list.
type WyomingConfig struct {
	Enabled           bool         `koanf:"enabled"`
	Host              string       `koanf:"host"`
	Port              int          `koanf:"port"`
	Languages         string       `koanf:"languages"`
	Speed             float64      `koanf:"speed"`
	Streaming         wyoming.Mode `koanf:"streaming"`
	StreamingMaxChars int          `koanf:"streamingmaxchars"`
}

// RecordingsConfig turns on keeping the audio of each successful
// transcription in Dir, an absolute path created at startup when missing.
type RecordingsConfig struct {
	Enable bool   `koanf:"enable"`
	Dir    string `koanf:"dir"`
}

type ShutdownTimingConfig struct {
	Total time.Duration `koanf:"total"`
	Grace time.Duration `koanf:"grace"`
	Hard  time.Duration `koanf:"hard"`
}

var defaultConfig = ServiceConfig{
	Host: "127.0.0.1",
	Port: 8920,
	Upstream: UpstreamConfig{
		URL:            "http://127.0.0.1:8080",
		TTSModel:       "qwen3-tts",
		ASRModel:       "qwen3-asr",
		TTSBackend:     upstream.LlamaCPP,
		TTSConcurrency: 1,
	},
	Voices: VoicesConfig{
		Dir: "/var/lib/llama-swap-voice/voices",
	},
	TTS: TTSConfig{
		Lang:          "en",
		MaxInputChars: 4096,
	},
	ASR: ASRConfig{
		PrefixRegex: `^language \w+<asr_text>`,
	},
	Request: RequestConfig{
		Timeout:          2 * time.Minute,
		MaxConcurrent:    4,
		MaxConcurrentTTS: 8,
	},
	Logger: LoggerConfig{
		Level:  observability.LevelInfo,
		Format: observability.FormatJSON,
	},
	ShutdownTiming: ShutdownTimingConfig{
		Total: 15 * time.Second,
		Grace: 0,
		Hard:  3 * time.Second,
	},
	Wyoming: WyomingConfig{
		Enabled:           false,
		Host:              "127.0.0.1",
		Port:              10300,
		Languages:         "en",
		Speed:             1.0,
		Streaming:         wyoming.ModeSentences,
		StreamingMaxChars: 300,
	},
}

func (c *ServiceConfig) Validate() error {
	errs := []error{
		c.validateListener(),
		c.validateUpstream(),
		c.validateVoices(),
		c.validateLimits(),
		c.validateLogger(),
		c.validateShutdown(),
		c.validateWyoming(),
		c.validateRecordings(),
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateListener() error {
	if c.Port < minPort || c.Port > maxPort {
		return fmt.Errorf("port must be between %d and %d, got %d", minPort, maxPort, c.Port)
	}

	return nil
}

func (c *ServiceConfig) validateUpstream() error {
	var errs []error

	parsed, err := url.Parse(c.Upstream.URL)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("upstream.url: %w", err))
	case !slices.Contains(upstreamSchemes, parsed.Scheme) || parsed.Host == "":
		errs = append(errs, fmt.Errorf("upstream.url must be an http or https URL with a host, got %q", c.Upstream.URL))
	}

	if c.Upstream.TTSModel == "" {
		errs = append(errs, errors.New("upstream.ttsmodel must not be empty"))
	}

	if c.Upstream.ASRModel == "" {
		errs = append(errs, errors.New("upstream.asrmodel must not be empty"))
	}

	if _, err := upstream.ParseBackend(string(c.Upstream.TTSBackend)); err != nil {
		errs = append(errs, err)
	}

	if c.Upstream.TTSConcurrency < minTTSConcurrency {
		errs = append(errs, fmt.Errorf("upstream.ttsconcurrency must be at least %d, got %d", minTTSConcurrency, c.Upstream.TTSConcurrency))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateVoices() error {
	found, err := voices.Discover(c.Voices.Dir)
	if c.Voices.Default == "" {
		return errors.Join(err, errors.New("voices.default must name a voice"))
	}

	if _, ok := found[c.Voices.Default]; !ok && len(found) > 0 {
		err = errors.Join(err, fmt.Errorf("voices.default %q is not a voice in %s", c.Voices.Default, c.Voices.Dir))
	}

	return err
}

func (c *ServiceConfig) validateLimits() error {
	var errs []error

	if c.TTS.MaxInputChars <= 0 {
		errs = append(errs, fmt.Errorf("tts.maxinputchars must be positive, got %d", c.TTS.MaxInputChars))
	}

	if c.Request.MaxConcurrent <= 0 {
		errs = append(errs, fmt.Errorf("request.maxconcurrent must be positive, got %d", c.Request.MaxConcurrent))
	}

	if c.Request.MaxConcurrentTTS <= 0 {
		errs = append(errs, fmt.Errorf("request.maxconcurrenttts must be positive, got %d", c.Request.MaxConcurrentTTS))
	}

	if c.Request.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("request.timeout must be positive, got %s", c.Request.Timeout))
	}

	if _, err := regexp.Compile(c.ASR.PrefixRegex); err != nil {
		errs = append(errs, fmt.Errorf("asr.prefixregex: %w", err))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateLogger() error {
	var errs []error

	if !slices.Contains(logLevels, c.Logger.Level) {
		errs = append(errs, fmt.Errorf("log.level must be one of %v, got %q", logLevels, c.Logger.Level))
	}

	if !slices.Contains(logFormats, c.Logger.Format) {
		errs = append(errs, fmt.Errorf("log.format must be one of %v, got %q", logFormats, c.Logger.Format))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateShutdown() error {
	var errs []error

	if c.ShutdownTiming.Total <= 0 {
		errs = append(errs, fmt.Errorf("shutdown.total must be positive, got %s", c.ShutdownTiming.Total))
	}

	if c.ShutdownTiming.Grace < 0 {
		errs = append(errs, fmt.Errorf("shutdown.grace must not be negative, got %s", c.ShutdownTiming.Grace))
	}

	if c.ShutdownTiming.Hard <= 0 {
		errs = append(errs, fmt.Errorf("shutdown.hard must be positive, got %s", c.ShutdownTiming.Hard))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateWyoming() error {
	w := &c.Wyoming
	if !w.Enabled {
		return nil
	}

	var errs []error

	if w.Port < minPort || w.Port > maxPort {
		errs = append(errs, fmt.Errorf("wyoming.port must be between %d and %d, got %d", minPort, maxPort, w.Port))
	}

	if w.Host == c.Host && w.Port == c.Port {
		errs = append(errs, fmt.Errorf("wyoming.host and wyoming.port must differ from host and port, both %s", net.JoinHostPort(c.Host, strconv.Itoa(c.Port))))
	}

	if slices.Contains(splitLanguages(w.Languages), "") {
		errs = append(errs, fmt.Errorf("wyoming.languages must be a comma-separated list of non-empty languages, got %q", w.Languages))
	}

	if math.IsNaN(w.Speed) || w.Speed < audio.MinSpeed || w.Speed > audio.MaxSpeed {
		errs = append(errs, fmt.Errorf("wyoming.speed must be between %g and %g, got %g", audio.MinSpeed, audio.MaxSpeed, w.Speed))
	}

	if !slices.Contains(streamingModes, w.Streaming) {
		errs = append(errs, fmt.Errorf("wyoming.streaming must be %s or %s, got %q", wyoming.ModeSentences, wyoming.ModeWhole, w.Streaming))
	}

	if w.StreamingMaxChars < minStreamingChars || w.StreamingMaxChars > c.TTS.MaxInputChars {
		errs = append(errs, fmt.Errorf("wyoming.streamingmaxchars must be between %d and %d, got %d", minStreamingChars, c.TTS.MaxInputChars, w.StreamingMaxChars))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateRecordings() error {
	if !c.Recordings.Enable {
		return nil
	}

	if !filepath.IsAbs(c.Recordings.Dir) {
		return fmt.Errorf("recordings.dir must be an absolute path when recordings.enable is true, got %q", c.Recordings.Dir)
	}

	return nil
}

// splitLanguages splits a comma-separated list and trims each entry, so an
// empty string gives one empty entry.
func splitLanguages(list string) []string {
	languages := strings.Split(list, languageSeparator)
	for i, language := range languages {
		languages[i] = strings.TrimSpace(language)
	}

	return languages
}

func (c *ServiceConfig) wyomingConfig(version string) wyoming.Config {
	return wyoming.Config{
		Languages:         splitLanguages(c.Wyoming.Languages),
		Speed:             c.Wyoming.Speed,
		Streaming:         c.Wyoming.Streaming,
		StreamingMaxChars: c.Wyoming.StreamingMaxChars,
		Version:           version,
	}
}

func (c *ServiceConfig) serverConfig(voiceFiles map[string]string) pipeline.Config {
	return pipeline.Config{
		TTSModel:         c.Upstream.TTSModel,
		ASRModel:         c.Upstream.ASRModel,
		Voices:           voiceFiles,
		DefaultVoice:     c.Voices.Default,
		Lang:             c.TTS.Lang,
		RequestTimeout:   c.Request.Timeout,
		MaxInputChars:    c.TTS.MaxInputChars,
		MaxConcurrent:    c.Request.MaxConcurrent,
		MaxConcurrentTTS: c.Request.MaxConcurrentTTS,
		TTSConcurrency:   c.Upstream.TTSConcurrency,
		ASRPrefixRegex:   c.ASR.PrefixRegex,
	}
}
