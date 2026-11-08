package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/config"
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
		"HOST":                    "0.0.0.0",
		"PORT":                    "9001",
		"UPSTREAM_URL":            "https://llama.example:8443",
		"UPSTREAM_TTSMODEL":       "tts-alt",
		"UPSTREAM_ASRMODEL":       "asr-alt",
		"UPSTREAM_TTSCONCURRENCY": "3",
		"VOICES_DIR":              "/srv/voices",
		"VOICES_DEFAULT":          "bm_fable",
		"TTS_LANG":                "de",
		"TTS_MAXINPUTCHARS":       "512",
		"ASR_PREFIXREGEX":         "^x",
		"REQUEST_TIMEOUT":         "45s",
		"REQUEST_MAXCONCURRENT":   "2",
		"LOG_LEVEL":               "debug",
		"LOG_FORMAT":              "console",
		"SHUTDOWN_TOTAL":          "20s",
		"SHUTDOWN_GRACE":          "5s",
		"SHUTDOWN_HARD":           "1s",
	}
	for key, value := range env {
		t.Setenv(envPrefix+key, value)
	}

	cfg, err := config.LoadConfig(envPrefix, defaultConfig)

	require.NoError(t, err)
	assert.Equal(t, ServiceConfig{
		Host:           "0.0.0.0",
		Port:           9001,
		Upstream:       UpstreamConfig{URL: "https://llama.example:8443", TTSModel: "tts-alt", ASRModel: "asr-alt", TTSConcurrency: 3},
		Voices:         VoicesConfig{Dir: "/srv/voices", Default: "bm_fable"},
		TTS:            TTSConfig{Lang: "de", MaxInputChars: 512},
		ASR:            ASRConfig{PrefixRegex: "^x"},
		Request:        RequestConfig{Timeout: 45 * time.Second, MaxConcurrent: 2},
		Logger:         LoggerConfig{Level: "debug", Format: "console"},
		ShutdownTiming: ShutdownTimingConfig{Total: 20 * time.Second, Grace: 5 * time.Second, Hard: time.Second},
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
}

func TestServiceConfig_ServerConfig_CopiesSettings(t *testing.T) {
	cfg := validConfig(t)
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
	assert.Equal(t, cfg.Upstream.TTSConcurrency, got.TTSConcurrency)
	assert.Equal(t, cfg.ASR.PrefixRegex, got.ASRPrefixRegex)
}
