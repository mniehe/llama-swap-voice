package config

import (
	"strings"

	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const (
	keyDelimiter = "."
	structTag    = "koanf"
	envDelimiter = "_"
)

func LoadConfig[T any](envPrefix string, defaults T) (T, error) {
	var loaded T
	k := koanf.New(keyDelimiter)

	if err := k.Load(structs.Provider(defaults, structTag), nil); err != nil {
		return loaded, err
	}

	if err := k.Load(env.Provider(envPrefix, keyDelimiter, envKey(envPrefix)), nil); err != nil {
		return loaded, err
	}

	if err := k.Unmarshal("", &loaded); err != nil {
		return loaded, err
	}

	return loaded, nil
}

func envKey(envPrefix string) func(string) string {
	return func(name string) string {
		trimmed := strings.ToLower(strings.TrimPrefix(name, envPrefix))
		return strings.ReplaceAll(trimmed, envDelimiter, keyDelimiter)
	}
}
