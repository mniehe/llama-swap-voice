package voices

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	hiddenPrefix  = "."
	maxVoiceBytes = 10 << 20
)

var extensions = []string{".wav", ".mp3"}

var ErrNoVoices = errors.New("no .wav or .mp3 voice files found")

// Symlinks are followed, so a link gives an existing clip an alias.
func Discover(dir string) (map[string]string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("voices directory %q: %w", dir, err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("voices directory: %w", err)
	}

	found := map[string]string{}
	var errs []error
	for _, entry := range entries {
		if err := add(found, root, entry.Name()); err != nil {
			errs = append(errs, err)
		}
	}

	if len(found) == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("voices directory %s: %w", root, ErrNoVoices))
	}

	return found, errors.Join(errs...)
}

func add(found map[string]string, root, fileName string) error {
	name, ok := voiceName(fileName)
	if !ok {
		return nil
	}

	path := filepath.Join(root, fileName)
	regular, err := isReadableFile(path)
	if err != nil {
		return fmt.Errorf("voice %q: %w", name, err)
	}

	if !regular {
		return nil
	}

	if existing, taken := found[name]; taken {
		return fmt.Errorf("voice %q is provided by both %s and %s", name, existing, path)
	}

	found[name] = path
	return nil
}

func voiceName(fileName string) (string, bool) {
	if strings.HasPrefix(fileName, hiddenPrefix) {
		return "", false
	}

	ext := filepath.Ext(fileName)
	for _, allowed := range extensions {
		if strings.EqualFold(ext, allowed) {
			return strings.TrimSuffix(fileName, ext), true
		}
	}

	return "", false
}

func isReadableFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	if !info.Mode().IsRegular() {
		return false, nil
	}

	if info.Size() > maxVoiceBytes {
		return false, fmt.Errorf("%s is %d bytes, larger than %d MiB", path, info.Size(), maxVoiceBytes>>20)
	}

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}

	return true, file.Close()
}
