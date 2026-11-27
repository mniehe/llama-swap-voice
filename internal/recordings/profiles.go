package recordings

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxProfiles bounds how many speaker profiles the store holds.
	MaxProfiles = 64
	// MaxProfileNameBytes bounds a profile's name after trimming.
	MaxProfileNameBytes = 64
	// ProfileIDLength is the length of a profile id: hex of eight random
	// bytes.
	ProfileIDLength = 2 * profileIDBytes

	profileIDBytes = 8
	profilesName   = "profiles.json"
	// maxProfilesBytes bounds the read of profiles.json. A profile takes its
	// 16-byte id, a name of at most 64 bytes escaped to at most six each, and
	// under 30 bytes of JSON around them: about 420 bytes, so 64 of them fit
	// in 27 KiB.
	maxProfilesBytes = 32 << 10
)

// ErrDuplicateName refuses a profile name another profile has.
var ErrDuplicateName = errors.New("another profile has that name")

var profileIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Profile is a named person whom recordings may be assigned to.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// profilesFile is the content of profiles.json.
type profilesFile struct {
	Profiles []Profile `json:"profiles"`
}

// Profiles returns the speaker profiles in the order they were added.
func (s *Store) Profiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.profiles)
}

// AddProfile adds a profile named name, trimmed, and returns it with the id
// the store gave it.
func (s *Store) AddProfile(name string) (Profile, error) {
	name, err := checkProfileName(name)
	if err != nil {
		return Profile{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.profiles) >= MaxProfiles {
		return Profile{}, fmt.Errorf("%w: the store holds at most %d profiles", ErrInvalid, MaxProfiles)
	}

	if s.nameTaken(name, "") {
		return Profile{}, fmt.Errorf("%w: %q", ErrDuplicateName, name)
	}

	id, err := s.freshProfileID()
	if err != nil {
		return Profile{}, err
	}

	profile := Profile{ID: id, Name: name}
	if err := s.saveProfiles(append(slices.Clone(s.profiles), profile)); err != nil {
		return Profile{}, err
	}

	return profile, nil
}

// RenameProfile gives the profile id the name name, trimmed. Recordings hold
// the id, so every recording shows the new name.
func (s *Store) RenameProfile(id, name string) error {
	if !profileIDPattern.MatchString(id) {
		return ErrNotFound
	}

	name, err := checkProfileName(name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	at := s.profileIndex(id)
	if at < 0 {
		return ErrNotFound
	}

	if s.nameTaken(name, id) {
		return fmt.Errorf("%w: %q", ErrDuplicateName, name)
	}

	profiles := slices.Clone(s.profiles)
	profiles[at].Name = name
	return s.saveProfiles(profiles)
}

// DeleteProfile deletes the profile id. It rewrites only profiles.json: a
// recording that names the profile reads without it, and drops it the next
// time its metadata is written.
func (s *Store) DeleteProfile(id string) error {
	if !profileIDPattern.MatchString(id) {
		return ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	at := s.profileIndex(id)
	if at < 0 {
		return ErrNotFound
	}

	return s.saveProfiles(slices.Delete(slices.Clone(s.profiles), at, at+1))
}

// checkProfileName returns name trimmed, or ErrInvalid when it is empty,
// over MaxProfileNameBytes, not valid UTF-8 or holds a control character.
func checkProfileName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: a profile name must be valid UTF-8", ErrInvalid)
	}

	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: a profile name may not be empty", ErrInvalid)
	case len(name) > MaxProfileNameBytes:
		return "", fmt.Errorf("%w: a profile name is at most %d bytes, got %d", ErrInvalid, MaxProfileNameBytes, len(name))
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: a profile name may not hold a control character", ErrInvalid)
	}

	return name, nil
}

// nameTaken reports whether a profile other than except has name, ignoring
// case. The caller holds mu.
func (s *Store) nameTaken(name, except string) bool {
	return slices.ContainsFunc(s.profiles, func(p Profile) bool {
		return p.ID != except && strings.EqualFold(p.Name, name)
	})
}

// profileIndex returns where the profile id is in the list, or -1. The
// caller holds mu.
func (s *Store) profileIndex(id string) int {
	return slices.IndexFunc(s.profiles, func(p Profile) bool { return p.ID == id })
}

// freshProfileID draws ids until one is not taken. The caller holds mu.
func (s *Store) freshProfileID() (string, error) {
	for range maxIDAttempts {
		var raw [profileIDBytes]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("pick profile id: %w", err)
		}

		id := hex.EncodeToString(raw[:])
		if s.profileIndex(id) < 0 {
			return id, nil
		}
	}

	return "", fmt.Errorf("pick profile id: %d draws were all taken", maxIDAttempts)
}

// saveProfiles writes profiles to profiles.json and makes them the store's.
// The caller holds mu.
func (s *Store) saveProfiles(profiles []Profile) error {
	data, err := json.Marshal(profilesFile{Profiles: profiles})
	if err != nil {
		return fmt.Errorf("encode %s: %w", profilesName, err)
	}

	if err := s.write(profilesName, data); err != nil {
		return err
	}

	s.profiles = profiles
	s.profilesBytes = int64(len(data))
	return nil
}

// loadProfiles reads profiles.json, which is absent until a profile is
// added. A file the store did not write as it is, over maxProfilesBytes or
// breaking a profile bound, fails Open: the store would otherwise drop the
// profiles on its next write.
func (s *Store) loadProfiles() error {
	file, err := os.Open(filepath.Join(s.dir, profilesName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", profilesName, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxProfilesBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", profilesName, err)
	}

	if len(data) > maxProfilesBytes {
		return fmt.Errorf("%s is over %d bytes", profilesName, maxProfilesBytes)
	}

	var stored profilesFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode %s: %w", profilesName, err)
	}

	if err := checkProfiles(stored.Profiles); err != nil {
		return fmt.Errorf("%s: %w", profilesName, err)
	}

	s.profiles = stored.Profiles
	s.profilesBytes = int64(len(data))
	return nil
}

// checkProfiles checks a list read back against every bound a write keeps.
func checkProfiles(profiles []Profile) error {
	if len(profiles) > MaxProfiles {
		return fmt.Errorf("%d profiles, over the %d allowed", len(profiles), MaxProfiles)
	}

	for i, profile := range profiles {
		if !profileIDPattern.MatchString(profile.ID) {
			return fmt.Errorf("profile id %q is not %d hex digits", profile.ID, ProfileIDLength)
		}

		name, err := checkProfileName(profile.Name)
		if err != nil {
			return err
		}

		if name != profile.Name {
			return fmt.Errorf("profile name %q has spaces around it", profile.Name)
		}

		for _, earlier := range profiles[:i] {
			if earlier.ID == profile.ID || strings.EqualFold(earlier.Name, profile.Name) {
				return fmt.Errorf("profile %q repeats an id or a name", profile.Name)
			}
		}
	}

	return nil
}

// assignable returns ids without repeats, or ErrInvalid when one names no
// profile. The caller holds mu.
func (s *Store) assignable(ids []string) ([]string, error) {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if s.profileIndex(id) < 0 {
			return nil, fmt.Errorf("%w: no profile has the id %q", ErrInvalid, id)
		}

		if !slices.Contains(kept, id) {
			kept = append(kept, id)
		}
	}

	return kept, nil
}

// known returns the ids that still name a profile. The caller holds mu.
func (s *Store) known(ids []string) []string {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if s.profileIndex(id) >= 0 && !slices.Contains(kept, id) {
			kept = append(kept, id)
		}
	}

	return kept
}

// resolve returns the profiles that ids name, leaving out an id whose
// profile was deleted. The caller holds mu.
func (s *Store) resolve(ids []string) []Profile {
	profiles := make([]Profile, 0, len(ids))
	for _, id := range s.known(ids) {
		profiles = append(profiles, s.profiles[s.profileIndex(id)])
	}

	return profiles
}
