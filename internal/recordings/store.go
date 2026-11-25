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
	"io"
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

// Retention is how long a recording nobody kept stays in the store.
const Retention = 7 * 24 * time.Hour

const (
	maxStoreBytes   = 1 << 30
	collectInterval = time.Hour
	// maxMetadataBytes bounds a metadata read. A transcript at the
	// upstream's 1 MiB bound, each byte escaped to at most six in JSON, fits.
	maxMetadataBytes = 7 << 20
	// maxIDAttempts bounds the redraws of an id already in the store, which
	// a 32-bit random suffix makes all but impossible.
	maxIDAttempts = 8

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

	errCorrupt = errors.New("corrupt recording metadata")

	idPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)
	// tempPattern matches only the names os.CreateTemp gives the probe and
	// writeAtomic's files, so Collect never deletes a file it did not make.
	tempPattern = regexp.MustCompile(`^(probe-\d+|\d{8}T\d{6}Z-[0-9a-f]{8}\.(opus|json)\.\d+)\.tmp$`)
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

// metadata is the JSON file saved beside each recording. At marshals as
// RFC 3339 with nanoseconds, so recordings made in one second keep their
// order.
type metadata struct {
	At         time.Time `json:"at"`
	DurationMs int64     `json:"durationMs"`
	Source     string    `json:"source"`
	Transcript string    `json:"transcript"`
	Keep       bool      `json:"keep"`
}

// indexed is what the store remembers of a complete recording, everything
// but the transcript, which stays on disk until a page shows it.
type indexed struct {
	id         string
	at         time.Time
	durationMs int64
	source     string
	keep       bool
	opusBytes  int64
	jsonBytes  int64
}

func (e *indexed) bytes() int64 {
	return e.opusBytes + e.jsonBytes
}

// Store owns a directory of recordings. One mutex serialises saves, the keep
// toggle and collection, so none of them sees another half done, and guards
// the index of what is on disk.
type Store struct {
	dir      string
	now      func() time.Time
	log      *slog.Logger
	maxBytes int64
	// ticks, write and newID are fields so that a test can drive collection
	// by hand, fail a write and force an id collision.
	ticks func() (<-chan time.Time, func())
	write func(name string, data []byte) error
	newID func(at time.Time) (string, error)

	mu    sync.Mutex
	byID  map[string]*indexed
	order []*indexed // oldest first, by at and then id
}

// Open creates dir if it is missing, checks that it can be written, indexes
// the recordings in it, deletes any whose metadata does not decode, and
// collects.
func Open(dir string, now func() time.Time, log *slog.Logger) (*Store, error) {
	s := &Store{
		dir:      dir,
		now:      now,
		log:      log,
		maxBytes: maxStoreBytes,
		ticks:    newTicks,
		newID:    randomID,
		byID:     map[string]*indexed{},
	}
	s.write = s.writeAtomic

	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create recordings directory: %w", err)
	}

	if err := probeWrite(dir); err != nil {
		return nil, err
	}

	if err := s.load(); err != nil {
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

// load indexes every recording that has both its files. Collect then deletes
// the files that belong to no indexed recording.
func (s *Store) load() error {
	sizes, err := s.fileSizes()
	if err != nil {
		return err
	}

	for name, jsonBytes := range sizes {
		id, ok := strings.CutSuffix(name, jsonSuffix)
		if !ok || !ValidID(id) {
			continue
		}

		opusBytes, haveOpus := sizes[id+opusSuffix]
		if !haveOpus {
			continue
		}

		meta, readErr := s.readMetadata(id)
		if errors.Is(readErr, errCorrupt) {
			if err := s.deleteFiles(id); err != nil {
				return err
			}
			s.log.Warn("deleted unreadable recording", slog.String("id", id), slog.Any("error", readErr))
			continue
		}
		if readErr != nil {
			return readErr
		}

		s.insert(&indexed{
			id:         id,
			at:         meta.At,
			durationMs: meta.DurationMs,
			source:     meta.Source,
			keep:       meta.Keep,
			opusBytes:  opusBytes,
			jsonBytes:  jsonBytes,
		})
	}

	return nil
}

// fileSizes maps the name of each regular file in the store to its size.
func (s *Store) fileSizes() (map[string]int64, error) {
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

	return sizes, nil
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

	at := s.now().UTC()
	id, err := s.freshID(at)
	if err != nil {
		return err
	}

	rec := &indexed{
		id:         id,
		at:         at,
		durationMs: int64(len(pcm)/pcmBytes) * time.Second.Milliseconds() / audio.RecordingRate,
		source:     source,
	}
	meta, err := json.Marshal(metadata{At: at, DurationMs: rec.durationMs, Source: source, Transcript: transcript})
	if err != nil {
		return fmt.Errorf("encode recording metadata: %w", err)
	}
	rec.opusBytes, rec.jsonBytes = int64(len(opus)), int64(len(meta))

	if err := s.makeRoom(rec.bytes()); err != nil {
		return err
	}

	if err := s.write(id+opusSuffix, opus); err != nil {
		return err
	}

	if err := s.write(id+jsonSuffix, meta); err != nil {
		return errors.Join(err, s.deleteFiles(id))
	}

	s.insert(rec)
	return nil
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

func randomID(at time.Time) (string, error) {
	var suffix [idSuffixLen]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("pick recording id: %w", err)
	}

	return at.Format(idTime) + "-" + hex.EncodeToString(suffix[:]), nil
}

// freshID draws ids until one is not in the store, so a save never replaces
// a recording. The caller holds mu.
func (s *Store) freshID(at time.Time) (string, error) {
	for range maxIDAttempts {
		id, err := s.newID(at)
		if err != nil {
			return "", err
		}
		if _, taken := s.byID[id]; !taken {
			return id, nil
		}
	}

	return "", fmt.Errorf("pick recording id: %d draws were all taken", maxIDAttempts)
}

// makeRoom deletes the oldest unkept recordings until size more bytes fit
// under the cap. It deletes nothing when kept recordings alone leave no room.
// The caller holds mu.
func (s *Store) makeRoom(size int64) error {
	var total, kept int64
	for _, rec := range s.order {
		total += rec.bytes()
		if rec.keep {
			kept += rec.bytes()
		}
	}

	if kept+size > s.maxBytes {
		return fmt.Errorf("%w: %d bytes kept, %d more would pass the %d-byte cap", ErrFull, kept, size, s.maxBytes)
	}

	var evict []string
	for _, rec := range s.order {
		if total+size <= s.maxBytes {
			break
		}
		if rec.keep {
			continue
		}
		evict = append(evict, rec.id)
		total -= rec.bytes()
	}

	for _, id := range evict {
		if err := s.remove(id); err != nil {
			return err
		}
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

// List returns at most limit recordings older than the recording before,
// newest first, or the newest ones when before is empty, and how many
// recordings the store holds.
func (s *Store) List(before string, limit int) ([]Recording, int, error) {
	if before != "" && !ValidID(before) {
		return nil, 0, ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	end := len(s.order)
	if before != "" {
		end = s.olderThan(before)
	}
	first := max(0, end-max(0, limit))

	list := make([]Recording, 0, end-first)
	for i := end - 1; i >= first; i-- {
		rec, err := s.recording(s.order[i])
		if err != nil {
			return nil, 0, err
		}
		list = append(list, rec)
	}

	return list, len(s.order), nil
}

// olderThan returns how many recordings sort before the recording id. A
// recording deleted since a page linked to it is placed by the time in its
// id, which is its save time cut to the second. The caller holds mu.
func (s *Store) olderThan(id string) int {
	cursor, ok := s.byID[id]
	if !ok {
		at, err := time.Parse(idTime, id[:len(idTime)])
		if err != nil {
			return 0
		}
		cursor = &indexed{id: id, at: at}
	}

	position, _ := slices.BinarySearchFunc(s.order, cursor, compareIndexed)
	return position
}

// recording reads the transcript of rec from its metadata. The caller holds
// mu.
func (s *Store) recording(rec *indexed) (Recording, error) {
	meta, err := s.readMetadata(rec.id)
	if err != nil {
		return Recording{}, err
	}

	return Recording{
		ID:         rec.id,
		At:         rec.at,
		Duration:   time.Duration(rec.durationMs) * time.Millisecond,
		Source:     rec.source,
		Transcript: meta.Transcript,
		Keep:       rec.keep,
	}, nil
}

func compareIndexed(a, b *indexed) int {
	if c := a.at.Compare(b.at); c != 0 {
		return c
	}

	return strings.Compare(a.id, b.id)
}

// insert adds rec to the index in order. The caller holds mu.
func (s *Store) insert(rec *indexed) {
	at, _ := slices.BinarySearchFunc(s.order, rec, compareIndexed)
	s.order = slices.Insert(s.order, at, rec)
	s.byID[rec.id] = rec
}

// forget drops id from the index. The caller holds mu.
func (s *Store) forget(id string) {
	rec, ok := s.byID[id]
	if !ok {
		return
	}

	if at, found := slices.BinarySearchFunc(s.order, rec, compareIndexed); found {
		s.order = slices.Delete(s.order, at, at+1)
	}
	delete(s.byID, id)
}

// Audio opens the Ogg Opus file of the recording id. The caller closes it.
func (s *Store) Audio(id string) (*os.File, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return nil, ErrNotFound
	}

	file, err := os.Open(filepath.Join(s.dir, id+opusSuffix))
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

	rec, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}

	meta, err := s.readMetadata(id)
	if err != nil {
		return err
	}

	meta.Keep = keep
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode recording metadata: %w", err)
	}

	if err := s.write(id+jsonSuffix, data); err != nil {
		return err
	}

	rec.keep = keep
	rec.jsonBytes = int64(len(data))
	return nil
}

// Collect deletes every recording nobody kept that is older than the
// retention, then every file that an interrupted save or delete left: a
// temporary file of the store's, or one half of a recording.
func (s *Store) Collect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.now().Add(-Retention)
	var expired []string
	for _, rec := range s.order {
		if !rec.keep && rec.at.Before(cutoff) {
			expired = append(expired, rec.id)
		}
	}

	for _, id := range expired {
		if err := s.remove(id); err != nil {
			return err
		}
	}

	if len(expired) > 0 {
		s.log.Info("expired recordings deleted", slog.Int("count", len(expired)))
	}

	return s.sweep()
}

// sweep deletes the store's leftover files. The caller holds mu, so no save
// is writing one of them.
func (s *Store) sweep() error {
	sizes, err := s.fileSizes()
	if err != nil {
		return err
	}

	deleted := 0
	for name := range sizes {
		if !s.leftover(name) {
			continue
		}

		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove leftover recording file: %w", err)
		}
		deleted++
	}

	if deleted > 0 {
		s.log.Info("leftover recording files deleted", slog.Int("count", deleted))
	}

	return nil
}

// leftover reports whether name is a temporary file of the store's or half
// of a recording the index does not hold. The caller holds mu.
func (s *Store) leftover(name string) bool {
	if tempPattern.MatchString(name) {
		return true
	}

	for _, suffix := range []string{opusSuffix, jsonSuffix} {
		if id, ok := strings.CutSuffix(name, suffix); ok && ValidID(id) {
			_, held := s.byID[id]
			return !held
		}
	}

	return false
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

// remove deletes the files of id and drops it from the index. The caller
// holds mu.
func (s *Store) remove(id string) error {
	if err := s.deleteFiles(id); err != nil {
		return err
	}

	s.forget(id)
	return nil
}

// deleteFiles deletes the metadata first, so a crash between the two leaves
// an orphan audio file that the next Collect removes, never a listed
// recording without audio.
func (s *Store) deleteFiles(id string) error {
	for _, suffix := range []string{jsonSuffix, opusSuffix} {
		err := os.Remove(filepath.Join(s.dir, id+suffix))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete recording %s: %w", id, err)
		}
	}

	return nil
}

// readMetadata reads the metadata of id through maxMetadataBytes. A file
// that is too long or does not decode is errCorrupt.
func (s *Store) readMetadata(id string) (metadata, error) {
	var meta metadata
	file, err := os.Open(filepath.Join(s.dir, id+jsonSuffix))
	if err != nil {
		return meta, fmt.Errorf("read recording metadata %s: %w", id, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return meta, fmt.Errorf("read recording metadata %s: %w", id, err)
	}

	if len(data) > maxMetadataBytes {
		return meta, fmt.Errorf("%w: %s is over %d bytes", errCorrupt, id, maxMetadataBytes)
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("%w: %s: %w", errCorrupt, id, err)
	}

	return meta, nil
}
