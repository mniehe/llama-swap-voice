package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPrefix = "LSV_TEST_"

type nested struct {
	Timeout time.Duration `koanf:"timeout"`
	Enabled bool          `koanf:"enabled"`
}

type sample struct {
	Name   string `koanf:"name"`
	Port   int    `koanf:"port"`
	Nested nested `koanf:"nested"`
}

var sampleDefaults = sample{
	Name:   "default",
	Port:   8920,
	Nested: nested{Timeout: 2 * time.Minute, Enabled: true},
}

func TestLoadConfig_Defaults_ReturnsDefaultValues(t *testing.T) {
	loaded, err := LoadConfig(testPrefix, sampleDefaults)

	require.NoError(t, err)
	assert.Equal(t, sampleDefaults, loaded)
}

func TestLoadConfig_Environment_OverridesNestedValues(t *testing.T) {
	t.Setenv(testPrefix+"NAME", "override")
	t.Setenv(testPrefix+"PORT", "9000")
	t.Setenv(testPrefix+"NESTED_TIMEOUT", "45s")
	t.Setenv(testPrefix+"NESTED_ENABLED", "false")

	loaded, err := LoadConfig(testPrefix, sampleDefaults)

	require.NoError(t, err)
	assert.Equal(t, sample{Name: "override", Port: 9000, Nested: nested{Timeout: 45 * time.Second}}, loaded)
}

func TestLoadConfig_OtherPrefix_IsIgnored(t *testing.T) {
	t.Setenv("OTHER_NAME", "ignored")

	loaded, err := LoadConfig(testPrefix, sampleDefaults)

	require.NoError(t, err)
	assert.Equal(t, "default", loaded.Name)
}

func TestLoadConfig_BadDuration_ReturnsError(t *testing.T) {
	t.Setenv(testPrefix+"NESTED_TIMEOUT", "soon")

	_, err := LoadConfig(testPrefix, sampleDefaults)

	require.Error(t, err)
}
