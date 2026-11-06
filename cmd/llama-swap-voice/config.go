package main

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/observability"
	"github.com/mniehe/llama-swap-voice/internal/server"
	"github.com/mniehe/llama-swap-voice/internal/voices"
)

var Version = "0.0.0-dev"

const (
	minPort = 1
	maxPort = 65535
)

var (
	upstreamSchemes = []string{"http", "https"}
	logLevels       = []string{observability.LevelDebug, observability.LevelInfo, observability.LevelWarn, observability.LevelError}
	logFormats      = []string{observability.FormatJSON, observability.FormatConsole}
)

type ServiceConfig struct {
	Host           string               `koanf:"host"`
	Port           int                  `koanf:"port"`
	Upstream       UpstreamConfig       `koanf:"upstream"`
	Voices         VoicesConfig         `koanf:"voices"`
	TTS            TTSConfig            `koanf:"tts"`
	ASR            ASRConfig            `koanf:"asr"`
	FFmpeg         string               `koanf:"ffmpeg"`
	Request        RequestConfig        `koanf:"request"`
	Logger         LoggerConfig         `koanf:"log"`
	ShutdownTiming ShutdownTimingConfig `koanf:"shutdown"`
}

type UpstreamConfig struct {
	URL      string `koanf:"url"`
	TTSModel string `koanf:"ttsmodel"`
	ASRModel string `koanf:"asrmodel"`
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
	PrefixRegex    string `koanf:"prefixregex"`
	TranscodeInput bool   `koanf:"transcodeinput"`
}

type RequestConfig struct {
	Timeout       time.Duration `koanf:"timeout"`
	MaxConcurrent int           `koanf:"maxconcurrent"`
}

type LoggerConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
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
		URL:      "http://127.0.0.1:8080",
		TTSModel: "qwen3-tts",
		ASRModel: "qwen3-asr",
	},
	Voices: VoicesConfig{
		Dir: "/var/lib/llama-swap-voice/voices",
	},
	TTS: TTSConfig{
		Lang:          "en",
		MaxInputChars: 4096,
	},
	ASR: ASRConfig{
		PrefixRegex:    `^language \w+<asr_text>`,
		TranscodeInput: true,
	},
	FFmpeg: "ffmpeg",
	Request: RequestConfig{
		Timeout:       2 * time.Minute,
		MaxConcurrent: 4,
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
}

func (c *ServiceConfig) Validate() error {
	errs := []error{
		c.validateListener(),
		c.validateUpstream(),
		c.validateVoices(),
		c.validateLimits(),
		c.validateTools(),
		c.validateLogger(),
		c.validateShutdown(),
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

	if c.Request.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("request.timeout must be positive, got %s", c.Request.Timeout))
	}

	if _, err := regexp.Compile(c.ASR.PrefixRegex); err != nil {
		errs = append(errs, fmt.Errorf("asr.prefixregex: %w", err))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) validateTools() error {
	if _, err := exec.LookPath(c.FFmpeg); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	return nil
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

	if c.ShutdownTiming.Hard < 0 {
		errs = append(errs, fmt.Errorf("shutdown.hard must not be negative, got %s", c.ShutdownTiming.Hard))
	}

	return errors.Join(errs...)
}

func (c *ServiceConfig) serverConfig(voiceFiles map[string]string) server.Config {
	return server.Config{
		TTSModel:          c.Upstream.TTSModel,
		ASRModel:          c.Upstream.ASRModel,
		Voices:            voiceFiles,
		DefaultVoice:      c.Voices.Default,
		Lang:              c.TTS.Lang,
		FFmpeg:            c.FFmpeg,
		RequestTimeout:    c.Request.Timeout,
		MaxInputChars:     c.TTS.MaxInputChars,
		MaxConcurrent:     c.Request.MaxConcurrent,
		ASRPrefixRegex:    c.ASR.PrefixRegex,
		TranscodeASRInput: c.ASR.TranscodeInput,
	}
}
