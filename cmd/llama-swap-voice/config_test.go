package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/config"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
	"github.com/mniehe/llama-swap-voice/internal/wyoming"
)

func voicesDir(t *testing.T, names ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("clip"), 0o600))
	}

	return dir
}

func validConfig(t *testing.T) ServiceConfig {
	t.Helper()

	cfg := defaultConfig
	cfg.Voices = VoicesConfig{Dir: voicesDir(t, "alice.mp3"), Default: "alice"}

	return cfg
}

func TestServiceConfig_Validate_ValidConfigPasses(t *testing.T) {
	cfg := validConfig(t)

	require.NoError(t, cfg.Validate())
}

func TestServiceConfig_Validate_RejectsInvalidValues(t *testing.T) {
	tests := map[string]struct {
		edit func(*ServiceConfig)
		want string
	}{
		"port too low":          {func(c *ServiceConfig) { c.Port = 0 }, "port must be between"},
		"port too high":         {func(c *ServiceConfig) { c.Port = 65536 }, "port must be between"},
		"upstream no scheme":    {func(c *ServiceConfig) { c.Upstream.URL = "localhost:8080" }, "upstream.url"},
		"upstream ftp":          {func(c *ServiceConfig) { c.Upstream.URL = "ftp://host" }, "upstream.url"},
		"upstream unparsable":   {func(c *ServiceConfig) { c.Upstream.URL = "http://[::1" }, "upstream.url"},
		"tts model empty":       {func(c *ServiceConfig) { c.Upstream.TTSModel = "" }, "upstream.ttsmodel"},
		"asr model empty":       {func(c *ServiceConfig) { c.Upstream.ASRModel = "" }, "upstream.asrmodel"},
		"tts concurrency zero":  {func(c *ServiceConfig) { c.Upstream.TTSConcurrency = 0 }, "upstream.ttsconcurrency must be at least 1"},
		"tts backend unknown":   {func(c *ServiceConfig) { c.Upstream.TTSBackend = "other" }, `upstream.ttsbackend must be llamacpp or qwentts, got "other"`},
		"tts backend empty":     {func(c *ServiceConfig) { c.Upstream.TTSBackend = "" }, "upstream.ttsbackend"},
		"default missing":       {func(c *ServiceConfig) { c.Voices.Default = "" }, "voices.default must name a voice"},
		"default unknown":       {func(c *ServiceConfig) { c.Voices.Default = "alloy" }, `voices.default "alloy" is not a voice`},
		"voices dir missing":    {func(c *ServiceConfig) { c.Voices.Dir = "/nonexistent/voices" }, "voices directory"},
		"voices dir empty":      {func(c *ServiceConfig) { c.Voices.Dir = t.TempDir() }, "no .wav or .mp3"},
		"max input chars zero":  {func(c *ServiceConfig) { c.TTS.MaxInputChars = 0 }, "tts.maxinputchars"},
		"max concurrent zero":   {func(c *ServiceConfig) { c.Request.MaxConcurrent = 0 }, "request.maxconcurrent"},
		"timeout zero":          {func(c *ServiceConfig) { c.Request.Timeout = 0 }, "request.timeout"},
		"bad regex":             {func(c *ServiceConfig) { c.ASR.PrefixRegex = "(" }, "asr.prefixregex"},
		"bad log level":         {func(c *ServiceConfig) { c.Logger.Level = "trace" }, "log.level"},
		"bad log format":        {func(c *ServiceConfig) { c.Logger.Format = "xml" }, "log.format"},
		"shutdown total zero":   {func(c *ServiceConfig) { c.ShutdownTiming.Total = 0 }, "shutdown.total"},
		"shutdown grace below0": {func(c *ServiceConfig) { c.ShutdownTiming.Grace = -time.Second }, "shutdown.grace"},
		"shutdown hard below0":  {func(c *ServiceConfig) { c.ShutdownTiming.Hard = -time.Second }, "shutdown.hard"},
		"shutdown hard zero":    {func(c *ServiceConfig) { c.ShutdownTiming.Hard = 0 }, "shutdown.hard must be positive"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.edit(&cfg)

			err := cfg.Validate()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestServiceConfig_Validate_ReportsEveryFailure(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = 0
	cfg.Voices.Default = ""
	cfg.Request.Timeout = 0
	cfg.Logger.Format = "xml"

	err := cfg.Validate()

	require.Error(t, err)
	for _, want := range []string{"port", "voices.default", "request.timeout", "log.format"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestServiceConfig_Validate_EmptyPrefixRegexIsAllowed(t *testing.T) {
	cfg := validConfig(t)
	cfg.ASR.PrefixRegex = ""

	require.NoError(t, cfg.Validate())
}

func TestLoadConfig_Environment_MapsToServiceConfig(t *testing.T) {
	env := map[string]string{
		"HOST":                      "0.0.0.0",
		"PORT":                      "9001",
		"UPSTREAM_URL":              "https://llama.example:8443",
		"UPSTREAM_TTSMODEL":         "tts-alt",
		"UPSTREAM_ASRMODEL":         "asr-alt",
		"UPSTREAM_TTSBACKEND":       "qwentts",
		"UPSTREAM_TTSCONCURRENCY":   "3",
		"VOICES_DIR":                "/srv/voices",
		"VOICES_DEFAULT":            "bm_fable",
		"TTS_LANG":                  "de",
		"TTS_MAXINPUTCHARS":         "512",
		"ASR_PREFIXREGEX":           "^x",
		"REQUEST_TIMEOUT":           "45s",
		"REQUEST_MAXCONCURRENT":     "2",
		"REQUEST_MAXCONCURRENTTTS":  "6",
		"LOG_LEVEL":                 "debug",
		"LOG_FORMAT":                "console",
		"SHUTDOWN_TOTAL":            "20s",
		"SHUTDOWN_GRACE":            "5s",
		"SHUTDOWN_HARD":             "1s",
		"WYOMING_ENABLED":           "true",
		"WYOMING_HOST":              "::1",
		"WYOMING_PORT":              "10301",
		"WYOMING_LANGUAGES":         "en,de",
		"WYOMING_SPEED":             "1.25",
		"WYOMING_STREAMING":         "whole",
		"WYOMING_STREAMINGMAXCHARS": "120",
		"RECORDINGS_ENABLE":         "true",
		"RECORDINGS_DIR":            "/var/lib/llama-swap-voice/recordings",
	}
	for key, value := range env {
		t.Setenv(envPrefix+key, value)
	}

	cfg, err := config.LoadConfig(envPrefix, defaultConfig)

	require.NoError(t, err)
	assert.Equal(t, ServiceConfig{
		Host:           "0.0.0.0",
		Port:           9001,
		Upstream:       UpstreamConfig{URL: "https://llama.example:8443", TTSModel: "tts-alt", ASRModel: "asr-alt", TTSBackend: upstream.QwenTTS, TTSConcurrency: 3},
		Voices:         VoicesConfig{Dir: "/srv/voices", Default: "bm_fable"},
		TTS:            TTSConfig{Lang: "de", MaxInputChars: 512},
		ASR:            ASRConfig{PrefixRegex: "^x"},
		Request:        RequestConfig{Timeout: 45 * time.Second, MaxConcurrent: 2, MaxConcurrentTTS: 6},
		Logger:         LoggerConfig{Level: "debug", Format: "console"},
		ShutdownTiming: ShutdownTimingConfig{Total: 20 * time.Second, Grace: 5 * time.Second, Hard: time.Second},
		Wyoming:        WyomingConfig{Enabled: true, Host: "::1", Port: 10301, Languages: "en,de", Speed: 1.25, Streaming: wyoming.ModeWhole, StreamingMaxChars: 120},
		Recordings:     RecordingsConfig{Enable: true, Dir: "/var/lib/llama-swap-voice/recordings"},
	}, cfg)
}

func TestLoadConfig_FFmpegVariable_IsIgnored(t *testing.T) {
	dir := voicesDir(t, "alice.mp3")
	t.Setenv(envPrefix+"FFMPEG", "/nonexistent/ffmpeg")
	t.Setenv(envPrefix+"VOICES_DIR", dir)
	t.Setenv(envPrefix+"VOICES_DEFAULT", "alice")

	cfg, err := loadConfig()

	require.NoError(t, err)
	want := defaultConfig
	want.Voices = VoicesConfig{Dir: dir, Default: "alice"}
	assert.Equal(t, want, cfg)
}

func TestLoadConfig_NoEnvironment_ReturnsDefaults(t *testing.T) {
	cfg, err := config.LoadConfig(envPrefix, defaultConfig)

	require.NoError(t, err)
	assert.Equal(t, defaultConfig, cfg)
	assert.Equal(t, 1, cfg.Upstream.TTSConcurrency)
	assert.Equal(t, 8, cfg.Request.MaxConcurrentTTS)
	assert.Equal(t, upstream.LlamaCPP, cfg.Upstream.TTSBackend)
	assert.Equal(t, WyomingConfig{
		Enabled:           false,
		Host:              "127.0.0.1",
		Port:              10300,
		Languages:         "en",
		Speed:             1.0,
		Streaming:         wyoming.ModeSentences,
		StreamingMaxChars: 300,
	}, cfg.Wyoming)
	assert.Equal(t, RecordingsConfig{Enable: false, Dir: ""}, cfg.Recordings)
}

func TestLoadConfig_UnknownTTSBackend_FailsStartup(t *testing.T) {
	t.Setenv(envPrefix+"VOICES_DIR", voicesDir(t, "alice.mp3"))
	t.Setenv(envPrefix+"VOICES_DEFAULT", "alice")
	t.Setenv(envPrefix+"UPSTREAM_TTSBACKEND", "other")

	_, err := loadConfig()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "llamacpp")
	assert.Contains(t, err.Error(), "qwentts")
}

func TestLoadConfig_NonPositiveMaxConcurrentTTS_FailsStartup(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(envPrefix+"VOICES_DIR", voicesDir(t, "alice.mp3"))
			t.Setenv(envPrefix+"VOICES_DEFAULT", "alice")
			t.Setenv(envPrefix+"REQUEST_MAXCONCURRENTTTS", value)

			_, err := loadConfig()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "request.maxconcurrenttts must be positive, got "+value)
		})
	}
}

func TestServiceConfig_ServerConfig_CopiesSettings(t *testing.T) {
	cfg := validConfig(t)
	cfg.Request.MaxConcurrentTTS = 6
	voiceFiles := map[string]string{"alice": "/voices/alice.mp3"}

	got := cfg.serverConfig(voiceFiles)

	assert.Equal(t, cfg.Upstream.TTSModel, got.TTSModel)
	assert.Equal(t, cfg.Upstream.ASRModel, got.ASRModel)
	assert.Equal(t, voiceFiles, got.Voices)
	assert.Equal(t, "alice", got.DefaultVoice)
	assert.Equal(t, cfg.TTS.Lang, got.Lang)
	assert.Equal(t, cfg.Request.Timeout, got.RequestTimeout)
	assert.Equal(t, cfg.TTS.MaxInputChars, got.MaxInputChars)
	assert.Equal(t, cfg.Request.MaxConcurrent, got.MaxConcurrent)
	assert.Equal(t, cfg.Request.MaxConcurrentTTS, got.MaxConcurrentTTS)
	assert.Equal(t, cfg.Upstream.TTSConcurrency, got.TTSConcurrency)
	assert.Equal(t, cfg.ASR.PrefixRegex, got.ASRPrefixRegex)
}

// enabledWyoming is a valid configuration with the Wyoming listener on.
func enabledWyoming(t *testing.T) ServiceConfig {
	t.Helper()

	cfg := validConfig(t)
	cfg.Wyoming.Enabled = true

	return cfg
}

func TestServiceConfig_Validate_WyomingAccepts(t *testing.T) {
	tests := map[string]func(*ServiceConfig){
		"defaults":                   func(*ServiceConfig) {},
		"port 1":                     func(c *ServiceConfig) { c.Wyoming.Port = 1 },
		"port 65535":                 func(c *ServiceConfig) { c.Wyoming.Port = 65535 },
		"http port on another host":  func(c *ServiceConfig) { c.Wyoming.Host = "0.0.0.0"; c.Wyoming.Port = c.Port },
		"ipv6 loopback":              func(c *ServiceConfig) { c.Wyoming.Host = "::1" },
		"two languages":              func(c *ServiceConfig) { c.Wyoming.Languages = "en,de" },
		"languages with spaces":      func(c *ServiceConfig) { c.Wyoming.Languages = "en, de" },
		"slowest speed":              func(c *ServiceConfig) { c.Wyoming.Speed = 0.25 },
		"fastest speed":              func(c *ServiceConfig) { c.Wyoming.Speed = 4 },
		"whole":                      func(c *ServiceConfig) { c.Wyoming.Streaming = wyoming.ModeWhole },
		"sentences":                  func(c *ServiceConfig) { c.Wyoming.Streaming = wyoming.ModeSentences },
		"one streaming char":         func(c *ServiceConfig) { c.Wyoming.StreamingMaxChars = 1 },
		"streaming chars at the max": func(c *ServiceConfig) { c.Wyoming.StreamingMaxChars = c.TTS.MaxInputChars },
	}

	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := enabledWyoming(t)
			edit(&cfg)

			require.NoError(t, cfg.Validate())
		})
	}
}

func TestServiceConfig_Validate_WyomingRejects(t *testing.T) {
	tests := map[string]struct {
		edit func(*ServiceConfig)
		want string
	}{
		"port 0":                       {func(c *ServiceConfig) { c.Wyoming.Port = 0 }, "wyoming.port must be between 1 and 65535, got 0"},
		"port 65536":                   {func(c *ServiceConfig) { c.Wyoming.Port = 65536 }, "wyoming.port must be between 1 and 65535, got 65536"},
		"the http host and port":       {func(c *ServiceConfig) { c.Wyoming.Host = c.Host; c.Wyoming.Port = c.Port }, "wyoming.host and wyoming.port must differ from host and port"},
		"no languages":                 {func(c *ServiceConfig) { c.Wyoming.Languages = "" }, "wyoming.languages"},
		"blank language":               {func(c *ServiceConfig) { c.Wyoming.Languages = " " }, "wyoming.languages"},
		"empty language between":       {func(c *ServiceConfig) { c.Wyoming.Languages = "en,,de" }, "wyoming.languages"},
		"trailing comma":               {func(c *ServiceConfig) { c.Wyoming.Languages = "en," }, "wyoming.languages"},
		"speed too slow":               {func(c *ServiceConfig) { c.Wyoming.Speed = 0.24 }, "wyoming.speed must be between 0.25 and 4, got 0.24"},
		"speed too fast":               {func(c *ServiceConfig) { c.Wyoming.Speed = 4.01 }, "wyoming.speed must be between 0.25 and 4, got 4.01"},
		"speed NaN":                    {func(c *ServiceConfig) { c.Wyoming.Speed = math.NaN() }, "wyoming.speed must be between 0.25 and 4, got NaN"},
		"unknown streaming":            {func(c *ServiceConfig) { c.Wyoming.Streaming = "words" }, `wyoming.streaming must be sentences or whole, got "words"`},
		"empty streaming":              {func(c *ServiceConfig) { c.Wyoming.Streaming = "" }, "wyoming.streaming"},
		"no streaming chars":           {func(c *ServiceConfig) { c.Wyoming.StreamingMaxChars = 0 }, "wyoming.streamingmaxchars must be between 1 and 4096, got 0"},
		"streaming chars over the max": {func(c *ServiceConfig) { c.Wyoming.StreamingMaxChars = c.TTS.MaxInputChars + 1 }, "wyoming.streamingmaxchars must be between 1 and 4096, got 4097"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := enabledWyoming(t)
			tt.edit(&cfg)

			err := cfg.Validate()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadConfig_WyomingSpeedNaN_FailsStartup(t *testing.T) {
	t.Setenv(envPrefix+"VOICES_DIR", voicesDir(t, "alice.mp3"))
	t.Setenv(envPrefix+"VOICES_DEFAULT", "alice")
	t.Setenv(envPrefix+"WYOMING_ENABLED", "true")
	t.Setenv(envPrefix+"WYOMING_SPEED", "NaN")

	_, err := loadConfig()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wyoming.speed must be between 0.25 and 4, got NaN")
}

func TestServiceConfig_Validate_WyomingDisabledIsNotChecked(t *testing.T) {
	cfg := validConfig(t)
	cfg.Wyoming = WyomingConfig{Port: 0, Languages: "", Speed: 0, Streaming: "words", StreamingMaxChars: 0}

	require.NoError(t, cfg.Validate())
}

func TestServiceConfig_WyomingConfig_CopiesSettings(t *testing.T) {
	cfg := enabledWyoming(t)
	cfg.Wyoming.Languages = "en, de"
	cfg.Wyoming.Speed = 1.5
	cfg.Wyoming.Streaming = wyoming.ModeWhole
	cfg.Wyoming.StreamingMaxChars = 42

	got := cfg.wyomingConfig("9.9.9")

	assert.Equal(t, wyoming.Config{
		Languages:         []string{"en", "de"},
		Speed:             1.5,
		Streaming:         wyoming.ModeWhole,
		StreamingMaxChars: 42,
		Version:           "9.9.9",
	}, got)
}

func TestServiceConfig_Validate_RecordingsRejectsMissingOrRelativeDir(t *testing.T) {
	for name, dir := range map[string]string{"empty": "", "relative": "recordings", "dot relative": "./var/recordings"} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Recordings = RecordingsConfig{Enable: true, Dir: dir}

			err := cfg.Validate()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "recordings.dir must be an absolute path")
		})
	}
}

func TestServiceConfig_Validate_RecordingsAcceptsAbsoluteDir(t *testing.T) {
	cfg := validConfig(t)
	cfg.Recordings = RecordingsConfig{Enable: true, Dir: "/var/lib/llama-swap-voice/recordings"}

	require.NoError(t, cfg.Validate())
}

func TestServiceConfig_Validate_RecordingsDisabledIsNotChecked(t *testing.T) {
	cfg := validConfig(t)
	cfg.Recordings = RecordingsConfig{Enable: false, Dir: "relative"}

	require.NoError(t, cfg.Validate())
}
