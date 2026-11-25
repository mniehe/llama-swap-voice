// Package recordings keeps the audio of successful transcriptions as Ogg
// Opus beside a JSON metadata file, expires what nobody chose to keep, and
// holds the store under a size cap.
package recordings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	retention       = 7 * 24 * time.Hour
	maxStoreBytes   = 1 << 30
	collectInterval = time.Hour

	opusSuffix = ".opus"
	jsonSuffix = ".json"
	tmpSuffix  = ".tmp"
	probeName  = "probe-*" + tmpSuffix

	dirMode     = 0o700
	idTime      = "20060102T150405Z"
	idSuffixLen = 4
	pcmBytes    = 2
)

var (
	ErrNotFound = errors.New("recording not found")
	ErrFull     = errors.New("kept recordings fill the store")

	idPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)
)

// Recording describes one saved transcription.
type Recording struct {
	ID         string
	At         time.Time
	Duration   time.Duration
	Source     string
	Transcript string
	Keep       bool
}

// metadata is the JSON file saved beside each recording.
type metadata struct {
	At         time.Time `json:"at"`
	DurationMs int64     `json:"durationMs"`
	Source     string    `json:"source"`
	Transcript string    `json:"transcript"`
	Keep       bool      `json:"keep"`
}

// entry is a complete recording found on disk with its size in bytes.
type entry struct {
	id    string
	meta  metadata
	bytes int64
}

// Store owns a directory of recordings. One mutex serialises saves, the keep
// toggle and collection, so none of them sees another half done.
type Store struct {
	dir      string
	now      func() time.Time
	log      *slog.Logger
	maxBytes int64
	// ticks is a field so that a test can drive collection by hand.
	ticks func() (<-chan time.Time, func())

	mu sync.Mutex
}

// Open creates dir if it is missing, checks that it can be written, removes
// what an interrupted save left behind and collects expired recordings.
func Open(dir string, now func() time.Time, log *slog.Logger) (*Store, error) {
	s := &Store{dir: dir, now: now, log: log, maxBytes: maxStoreBytes, ticks: newTicks}

	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create recordings directory: %w", err)
	}

	if err := probeWrite(dir); err != nil {
		return nil, err
	}

	if err := s.removeLeftovers(); err != nil {
		return nil, err
	}

	if err := s.Collect(); err != nil {
		return nil, err
	}

	return s, nil
}

func newTicks() (ticks <-chan time.Time, stop func()) {
	ticker := time.NewTicker(collectInterval)
	return ticker.C, ticker.Stop
}

func probeWrite(dir string) error {
	probe, err := os.CreateTemp(dir, probeName)
	if err != nil {
		return fmt.Errorf("recordings directory is not writable: %w", err)
	}

	return errors.Join(probe.Close(), os.Remove(probe.Name()))
}

// ValidID reports whether id has the form Save gives it. Callers check an id
// from a request with it before the id reaches a path.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// Save encodes wav, the 16 kHz mono WAV the recogniser was sent, and writes
// it with its metadata. When the store would pass its cap it first deletes the
// oldest recordings nobody kept; it returns ErrFull and saves nothing when
// kept recordings alone leave no room.
func (s *Store) Save(wav []byte, source, transcript string) error {
	pcm, err := pcmOf(wav)
	if err != nil {
		return err
	}

	opus, err := encodeOpus(pcm)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	at := s.now().UTC().Truncate(time.Second)
	id, err := newID(at)
	if err != nil {
		return err
	}

	meta, err := json.Marshal(metadata{
		At:         at,
		DurationMs: int64(len(pcm)/pcmBytes) * time.Second.Milliseconds() / audio.RecordingRate,
		Source:     source,
		Transcript: transcript,
	})
	if err != nil {
		return fmt.Errorf("encode recording metadata: %w", err)
	}

	if err := s.makeRoom(int64(len(opus) + len(meta))); err != nil {
		return err
	}

	if err := s.writeAtomic(id+opusSuffix, opus); err != nil {
		return err
	}

	return s.writeAtomic(id+jsonSuffix, meta)
}

// pcmOf returns the samples of wav after checking that its header is the
// exact one audio.NormaliseUpload writes for 16 kHz mono 16-bit PCM.
func pcmOf(wav []byte) ([]byte, error) {
	empty, err := audio.WAVHeader(audio.RecordingRate, pcmBytes, 1, 0)
	if err != nil {
		return nil, err
	}

	headerBytes := len(empty)
	if len(wav) < headerBytes {
		return nil, errors.New("recording is shorter than a WAV header")
	}

	want, err := audio.WAVHeader(audio.RecordingRate, pcmBytes, 1, len(wav)-headerBytes)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(wav[:headerBytes], want) {
		return nil, fmt.Errorf("recording is not 16-bit mono PCM WAV at %d Hz", audio.RecordingRate)
	}

	return wav[headerBytes:], nil
}

func encodeOpus(pcm []byte) ([]byte, error) {
	var out bytes.Buffer
	encoder, err := audio.NewRecordingEncoder(&out)
	if err != nil {
		return nil, fmt.Errorf("start recording encoder: %w", err)
	}

	_, writeErr := encoder.Write(pcm)
	if err := errors.Join(writeErr, encoder.Close()); err != nil {
		return nil, fmt.Errorf("encode recording: %w", err)
	}

	return out.Bytes(), nil
}

func newID(at time.Time) (string, error) {
	var suffix [idSuffixLen]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("pick recording id: %w", err)
	}

	return at.Format(idTime) + "-" + hex.EncodeToString(suffix[:]), nil
}

// makeRoom deletes the oldest unkept recordings until size more bytes fit
// under the cap. It deletes nothing when kept recordings alone leave no room.
func (s *Store) makeRoom(size int64) error {
	entries, err := s.scan()
	if err != nil {
		return err
	}

	var total, kept int64
	for _, found := range entries {
		total += found.bytes
		if found.meta.Keep {
			kept += found.bytes
		}
	}

	if kept+size > s.maxBytes {
		return fmt.Errorf("%w: %d bytes kept, %d more would pass the %d-byte cap", ErrFull, kept, size, s.maxBytes)
	}

	slices.SortFunc(entries, oldestFirst)
	for _, found := range entries {
		if total+size <= s.maxBytes {
			break
		}
		if found.meta.Keep {
			continue
		}

		if err := s.remove(found.id); err != nil {
			return err
		}
		total -= found.bytes
	}

	return nil
}

// writeAtomic writes data to a temporary file in the store and renames it
// into place, so a crash leaves either the whole file or none of it.
func (s *Store) writeAtomic(name string, data []byte) error {
	file, err := os.CreateTemp(s.dir, name+".*"+tmpSuffix)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", name, err), os.Remove(file.Name()))
	}

	if err := os.Rename(file.Name(), filepath.Join(s.dir, name)); err != nil {
		return errors.Join(fmt.Errorf("rename %s: %w", name, err), os.Remove(file.Name()))
	}

	return nil
}

// List returns every complete recording, newest first.
func (s *Store) List() ([]Recording, error) {
	s.mu.Lock()
	entries, err := s.scan()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	slices.SortFunc(entries, oldestFirst)
	slices.Reverse(entries)

	list := make([]Recording, 0, len(entries))
	for _, found := range entries {
		list = append(list, found.recording())
	}

	return list, nil
}

func (e *entry) recording() Recording {
	return Recording{
		ID:         e.id,
		At:         e.meta.At,
		Duration:   time.Duration(e.meta.DurationMs) * time.Millisecond,
		Source:     e.meta.Source,
		Transcript: e.meta.Transcript,
		Keep:       e.meta.Keep,
	}
}

func oldestFirst(a, b *entry) int {
	if c := a.meta.At.Compare(b.meta.At); c != 0 {
		return c
	}

	return strings.Compare(a.id, b.id)
}

// Audio opens the Ogg Opus file of the recording id. The caller closes it.
func (s *Store) Audio(id string) (*os.File, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := os.Open(filepath.Join(s.dir, id+opusSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open recording %s: %w", id, err)
	}

	return file, nil
}

// SetKeep marks the recording id as kept, which exempts it from expiry and
// eviction, or clears the mark.
func (s *Store) SetKeep(id string, keep bool) error {
	if !ValidID(id) {
		return ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.readMetadata(id)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	meta.Keep = keep
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode recording metadata: %w", err)
	}

	return s.writeAtomic(id+jsonSuffix, data)
}

// Collect deletes every recording nobody kept that is older than the
// retention.
func (s *Store) Collect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.scan()
	if err != nil {
		return err
	}

	cutoff := s.now().Add(-retention)
	deleted := 0
	for _, found := range entries {
		if found.meta.Keep || !found.meta.At.Before(cutoff) {
			continue
		}

		if err := s.remove(found.id); err != nil {
			return err
		}
		deleted++
	}

	if deleted > 0 {
		s.log.Info("expired recordings deleted", slog.Int("count", deleted))
	}

	return nil
}

// Run collects every interval until ctx ends.
func (s *Store) Run(ctx context.Context) {
	ticks, stop := s.ticks()
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := s.Collect(); err != nil {
				s.log.Warn("could not collect expired recordings", slog.Any("error", err))
			}
		}
	}
}

// remove deletes the metadata first, so a crash between the two leaves an
// orphan audio file that the next Open removes, never a listed recording
// without audio.
func (s *Store) remove(id string) error {
	for _, suffix := range []string{jsonSuffix, opusSuffix} {
		err := os.Remove(filepath.Join(s.dir, id+suffix))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete recording %s: %w", id, err)
		}
	}

	return nil
}

// scan reads every recording that has both its files. The caller holds mu.
func (s *Store) scan() ([]*entry, error) {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read recordings directory: %w", err)
	}

	sizes := map[string]int64{}
	for _, file := range files {
		info, infoErr := file.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		sizes[file.Name()] = info.Size()
	}

	var entries []*entry
	for name, size := range sizes {
		id, ok := strings.CutSuffix(name, jsonSuffix)
		if !ok || !ValidID(id) {
			continue
		}

		opusBytes, haveOpus := sizes[id+opusSuffix]
		if !haveOpus {
			continue
		}

		meta, readErr := s.readMetadata(id)
		if readErr != nil {
			s.log.Warn("skipping unreadable recording metadata", slog.String("id", id), slog.Any("error", readErr))
			continue
		}

		entries = append(entries, &entry{id: id, meta: meta, bytes: size + opusBytes})
	}

	return entries, nil
}

func (s *Store) readMetadata(id string) (metadata, error) {
	var meta metadata
	data, err := os.ReadFile(filepath.Join(s.dir, id+jsonSuffix))
	if err != nil {
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("decode recording metadata %s: %w", id, err)
	}

	return meta, nil
}

// removeLeftovers deletes temporary files and any recording missing one of
// its two files, which only an interrupted save or delete leaves behind.
func (s *Store) removeLeftovers() error {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read recordings directory: %w", err)
	}

	present := map[string]bool{}
	for _, file := range files {
		present[file.Name()] = true
	}

	for _, file := range files {
		if !leftover(file.Name(), present) {
			continue
		}

		if err := os.Remove(filepath.Join(s.dir, file.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove leftover recording file: %w", err)
		}
	}

	return nil
}

func leftover(name string, present map[string]bool) bool {
	if strings.HasSuffix(name, tmpSuffix) {
		return true
	}

	if id, ok := strings.CutSuffix(name, opusSuffix); ok && ValidID(id) {
		return !present[id+jsonSuffix]
	}

	if id, ok := strings.CutSuffix(name, jsonSuffix); ok && ValidID(id) {
		return !present[id+opusSuffix]
	}

	return false
}
