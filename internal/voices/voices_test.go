package voices

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type voiceDir struct {
	files      []string
	links      map[string]string
	subdirs    []string
	unreadable []string
}

func (v *voiceDir) build(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, name := range v.files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("clip "+name), 0o600))
	}

	for link, target := range v.links {
		require.NoError(t, os.Symlink(target, filepath.Join(dir, link)))
	}

	for _, name := range v.subdirs {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o700))
	}

	for _, name := range v.unreadable {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("clip"), 0o000))
	}

	return dir
}

func TestDiscover_Directory_FindsVoices(t *testing.T) {
	tests := []struct {
		name  string
		dir   voiceDir
		voice map[string]string
	}{
		{
			name:  "wav and mp3",
			dir:   voiceDir{files: []string{"alice.mp3", "narrator.wav"}},
			voice: map[string]string{"alice": "alice.mp3", "narrator": "narrator.wav"},
		},
		{
			name:  "extension case ignored",
			dir:   voiceDir{files: []string{"alice.MP3", "narrator.Wav"}},
			voice: map[string]string{"alice": "alice.MP3", "narrator": "narrator.Wav"},
		},
		{
			name:  "symlink alias",
			dir:   voiceDir{files: []string{"alice.mp3"}, links: map[string]string{"bm_fable.mp3": "alice.mp3"}},
			voice: map[string]string{"alice": "alice.mp3", "bm_fable": "bm_fable.mp3"},
		},
		{
			name:  "other files ignored",
			dir:   voiceDir{files: []string{"alice.mp3", ".hidden.wav", "notes.txt", "clip.ogg", "README"}, subdirs: []string{"nested.wav"}},
			voice: map[string]string{"alice": "alice.mp3"},
		},
		{
			name:  "symlink to directory ignored",
			dir:   voiceDir{files: []string{"alice.mp3"}, subdirs: []string{"store"}, links: map[string]string{"store.wav": "store"}},
			voice: map[string]string{"alice": "alice.mp3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.dir.build(t)

			found, err := Discover(dir)

			require.NoError(t, err)
			want := map[string]string{}
			for name, file := range tt.voice {
				want[name] = filepath.Join(dir, file)
			}
			assert.Equal(t, want, found)
		})
	}
}

func TestDiscover_Directory_ReportsProblems(t *testing.T) {
	tests := []struct {
		name     string
		dir      voiceDir
		contains []string
	}{
		{
			name:     "duplicate name",
			dir:      voiceDir{files: []string{"alice.mp3", "alice.wav", "narrator.wav", "narrator.MP3"}},
			contains: []string{`voice "alice" is provided by both`, `voice "narrator" is provided by both`},
		},
		{
			name:     "no voices",
			dir:      voiceDir{files: []string{"notes.txt"}},
			contains: []string{ErrNoVoices.Error()},
		},
		{
			name:     "dangling symlink",
			dir:      voiceDir{files: []string{"alice.mp3"}, links: map[string]string{"bm_fable.mp3": "missing.mp3"}},
			contains: []string{`voice "bm_fable"`, "no such file"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Discover(tt.dir.build(t))

			require.Error(t, err)
			for _, want := range tt.contains {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestDiscover_UnreadableFile_ReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}

	dir := (&voiceDir{files: []string{"alice.mp3"}, unreadable: []string{"locked.wav"}}).build(t)

	_, err := Discover(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `voice "locked"`)
}

func TestDiscover_MissingDirectory_ReturnsError(t *testing.T) {
	_, err := Discover(filepath.Join(t.TempDir(), "absent"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "voices directory")
}

func TestDiscover_RelativeDirectory_ReturnsAbsolutePaths(t *testing.T) {
	dir := (&voiceDir{files: []string{"alice.wav"}}).build(t)
	t.Chdir(dir)

	found, err := Discover(".")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"alice": filepath.Join(dir, "alice.wav")}, found)
}
