package recordings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// profileLimit and profileNameLimit are the profile bounds, written out
	// so a changed constant fails here.
	profileLimit     = 64
	profileNameLimit = 64
	profilesFileName = "profiles.json"
	unknownProfileID = "0123456789abcdef"
)

func profilesPath(dir string) string {
	return filepath.Join(dir, profilesFileName)
}

func addProfile(t *testing.T, store *Store, name string) Profile {
	t.Helper()

	profile, err := store.AddProfile(name)
	require.NoError(t, err)
	require.Len(t, profile.ID, ProfileIDLength)

	return profile
}

func profileNames(profiles []Profile) []string {
	found := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		found = append(found, profile.Name)
	}

	return found
}

func TestProfiles_AddRenameAndDeletePersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "  Anna  ")
	ben := addProfile(t, store, "Ben")
	carl := addProfile(t, store, "Carl")

	require.NoError(t, store.RenameProfile(ben.ID, "Benjamin"))
	require.NoError(t, store.DeleteProfile(carl.ID))

	reopened, _ := openStore(t, dir)
	assert.Equal(t, []Profile{{ID: anna.ID, Name: "Anna"}, {ID: ben.ID, Name: "Benjamin"}}, reopened.Profiles())
	assert.NotEqual(t, anna.ID, ben.ID)
}

func TestProfiles_NoneUntilOneIsAdded(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)

	assert.Empty(t, store.Profiles())
	assert.NoFileExists(t, profilesPath(dir), "the file is written only when a profile is")
}

func TestAddProfile_RefusesOneMoreThanTheLimit(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	for i := range profileLimit {
		addProfile(t, store, fmt.Sprintf("person %d", i))
	}
	before := fileBytes(t, profilesPath(dir))

	_, err := store.AddProfile("one too many")

	require.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, store.Profiles(), profileLimit)
	assert.Equal(t, before, fileBytes(t, profilesPath(dir)))
}

func TestAddProfile_NameAtTheLengthLimitIsAccepted(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	ascii := strings.Repeat("a", profileNameLimit)
	// Two-byte characters make the limit fall on a character boundary.
	accented := strings.Repeat("é", profileNameLimit/2)

	assert.Equal(t, ascii, addProfile(t, store, " "+ascii+"\t").Name, "the name is trimmed before it is measured")
	assert.Equal(t, accented, addProfile(t, store, accented).Name)
}

func TestAddProfile_RefusesBadNames(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)

	for name, value := range map[string]string{
		"one byte over":  strings.Repeat("a", profileNameLimit+1),
		"empty":          "",
		"only spaces":    "   ",
		"a tab":          "Anna\tBen",
		"a newline":      "Anna\nBen",
		"a DEL":          "Anna\x7f",
		"a C1 control":   "Anna\u0085Ben",
		"invalid UTF-8":  "Anna\xff",
		"a NUL":          "Anna\x00",
		"over in UTF-8":  strings.Repeat("é", profileNameLimit/2) + "a",
		"control inside": "An\u200bna\x1b",
	} {
		_, err := store.AddProfile(value)

		require.ErrorIs(t, err, ErrInvalid, name)
	}
	assert.Empty(t, store.Profiles())
	assert.NoFileExists(t, profilesPath(dir))
}

func TestProfiles_DuplicateNamesAreRefused(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	anna := addProfile(t, store, "Anna")
	ben := addProfile(t, store, "Ben")

	_, err := store.AddProfile("anna ")
	require.ErrorIs(t, err, ErrDuplicateName, "names differing in case or spaces are one name")
	require.ErrorIs(t, store.RenameProfile(ben.ID, "ANNA"), ErrDuplicateName)

	require.NoError(t, store.RenameProfile(anna.ID, "ANNA"), "a profile may change the case of its own name")
	assert.Equal(t, []string{"ANNA", "Ben"}, profileNames(store.Profiles()))
}

func TestRenameProfile_RefusesABadName(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	anna := addProfile(t, store, "Anna")

	require.ErrorIs(t, store.RenameProfile(anna.ID, strings.Repeat("a", profileNameLimit+1)), ErrInvalid)
	require.ErrorIs(t, store.RenameProfile(anna.ID, "An\nna"), ErrInvalid)
	assert.Equal(t, []string{"Anna"}, profileNames(store.Profiles()))
}

func TestProfiles_UnknownOrMalformedIDIsNotFound(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	addProfile(t, store, "Anna")

	for _, id := range []string{unknownProfileID, "", "x", "../profiles", strings.Repeat("A", ProfileIDLength)} {
		require.ErrorIs(t, store.RenameProfile(id, "Ben"), ErrNotFound, "rename %q", id)
		require.ErrorIs(t, store.DeleteProfile(id), ErrNotFound, "delete %q", id)
	}
	assert.Equal(t, []string{"Anna"}, profileNames(store.Profiles()))
}

func TestCollect_DeletesACrashLeftProfilesTempFileButNeverTheProfiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	addProfile(t, store, "Anna")
	written := fileBytes(t, profilesPath(dir))
	leftover := profilesFileName + ".123456" + tmpSuffix
	require.NoError(t, os.WriteFile(filepath.Join(dir, leftover), []byte("{"), 0o600))

	require.NoError(t, store.Collect())

	assert.Equal(t, []string{profilesFileName}, names(t, dir))
	assert.Equal(t, written, fileBytes(t, profilesPath(dir)))

	openStore(t, dir)
	assert.Equal(t, written, fileBytes(t, profilesPath(dir)), "Open sweeps and keeps it too")
}

func TestOpen_KeepsForeignProfilesTempNames(t *testing.T) {
	dir := t.TempDir()
	foreign := []string{"profiles.json.tmp", "profiles.json.x.tmp", "profiles.123.tmp", "profiles.json.1.tmp.bak"}
	for _, name := range foreign {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("mine"), 0o600))
	}

	openStore(t, dir)

	for _, name := range foreign {
		assert.FileExists(t, filepath.Join(dir, name))
	}
}

func TestSave_ProfilesCountTowardTheCap(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	addProfile(t, store, strings.Repeat("p", profileNameLimit))
	profileBytes := int64(len(fileBytes(t, profilesPath(dir))))
	first := save(t, store, "first")
	size := recordingBytes(t, dir, first)
	// Room for the profiles and one recording: a second evicts the first
	// only because the profiles count.
	store.maxBytes = profileBytes + 2*size - 1

	c.advance(time.Minute)
	second := save(t, store, "second")

	assert.Equal(t, []string{second.ID}, ids(t, store))
	assert.FileExists(t, profilesPath(dir))
}

func TestOpen_RefusesAnUnusableProfilesFile(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":       "{",
		"over the bound": `{"profiles":[]}` + strings.Repeat(" ", 64<<10),
		"a bad id":       `{"profiles":[{"id":"x","name":"Anna"}]}`,
		"a bad name":     `{"profiles":[{"id":"0123456789abcdef","name":""}]}`,
		"duplicate ids":  `{"profiles":[{"id":"0123456789abcdef","name":"Anna"},{"id":"0123456789abcdef","name":"Ben"}]}`,
		"duplicate name": `{"profiles":[{"id":"0123456789abcdef","name":"Anna"},{"id":"fedcba9876543210","name":"anna"}]}`,
	} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(profilesPath(dir), []byte(content), 0o600))

		_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))

		require.Error(t, err, name)
		assert.Contains(t, err.Error(), profilesFileName, name)
		assert.FileExists(t, profilesPath(dir), name)
	}
}

func TestOpen_RefusesMoreProfilesThanTheLimit(t *testing.T) {
	dir := t.TempDir()
	list := make([]Profile, 0, profileLimit+1)
	for i := range profileLimit + 1 {
		list = append(list, Profile{ID: fmt.Sprintf("%016x", i), Name: fmt.Sprintf("person %d", i)})
	}
	data, err := json.Marshal(map[string][]Profile{"profiles": list})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(profilesPath(dir), data, 0o600))

	_, openErr := Open(dir, time.Now, slog.New(slog.DiscardHandler))

	require.Error(t, openErr)
	assert.Contains(t, openErr.Error(), profilesFileName)
}

func TestAnnotate_ProfilesRoundTripThroughTheFileAndAReopen(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	ben := addProfile(t, store, "Ben")
	rec := save(t, store, heardText)
	transcriptBefore := transcriptOf(t, metadataPath(dir, rec.ID))

	require.NoError(t, store.Annotate(rec.ID, Annotation{Keep: true, Profiles: []string{ben.ID, anna.ID, ben.ID}}))

	assert.Equal(t, []string{ben.ID, anna.ID}, readMetadata(t, metadataPath(dir, rec.ID)).Profiles, "repeats are dropped")
	assert.Equal(t, transcriptBefore, transcriptOf(t, metadataPath(dir, rec.ID)))
	reopened, _ := openStore(t, dir)
	got, err := reopened.Recording(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, []Profile{ben, anna}, got.Profiles)
	list, _, err := reopened.List("", 10)
	require.NoError(t, err)
	assert.Equal(t, []Profile{ben, anna}, list[0].Profiles, "the list names the speakers too")
}

func TestSave_WritesAnEmptyProfileList(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fileBytes(t, metadataPath(dir, rec.ID)), &fields))

	assert.JSONEq(t, `[]`, string(fields["profiles"]))
}

func TestOpen_LoadsMetadataWrittenBeforeProfiles(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, []byte(preAnnotationMetadata))

	store, _ := openStore(t, dir)

	got, err := store.Recording(plantedCorruptID)
	require.NoError(t, err)
	assert.Equal(t, plantedCorruptID, got.ID)
	assert.Empty(t, got.Profiles)
}

func TestAnnotate_UnknownProfileIsRefused(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	rec := save(t, store, heardText)
	before := fileBytes(t, metadataPath(dir, rec.ID))

	for _, profiles := range [][]string{{unknownProfileID}, {anna.ID, unknownProfileID}, {"../x"}, {""}} {
		err := store.Annotate(rec.ID, Annotation{Keep: true, Profiles: profiles})

		require.ErrorIs(t, err, ErrInvalid, "profiles %q", profiles)
		assert.Equal(t, before, fileBytes(t, metadataPath(dir, rec.ID)), "profiles %q", profiles)
	}
}

func TestDeleteProfile_RecordingReadsWithoutItAndItsNextSaveDropsIt(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	ben := addProfile(t, store, "Ben")
	rec := save(t, store, heardText)
	require.NoError(t, store.Annotate(rec.ID, Annotation{Profiles: []string{anna.ID, ben.ID}}))
	metadataBefore := fileBytes(t, metadataPath(dir, rec.ID))

	require.NoError(t, store.DeleteProfile(anna.ID))

	assert.Equal(t, metadataBefore, fileBytes(t, metadataPath(dir, rec.ID)), "a delete rewrites only the profiles")
	got, err := store.Recording(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, []Profile{ben}, got.Profiles)

	require.NoError(t, store.SetKeep(rec.ID, true))
	assert.Equal(t, []string{ben.ID}, readMetadata(t, metadataPath(dir, rec.ID)).Profiles)
}

func TestRenameProfile_ShowsOnEveryRecording(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	anna := addProfile(t, store, "Anna")
	first := save(t, store, "first")
	second := save(t, store, "second")
	for _, rec := range []*Recording{first, second} {
		require.NoError(t, store.Annotate(rec.ID, Annotation{Profiles: []string{anna.ID}}))
	}

	require.NoError(t, store.RenameProfile(anna.ID, "Annabel"))

	for _, rec := range []*Recording{first, second} {
		got, err := store.Recording(rec.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{"Annabel"}, profileNames(got.Profiles))
	}
}

const (
	// profilesFileLimit is the read bound of profiles.json, written out so a
	// changed constant fails here.
	profilesFileLimit = 32 << 10
	oneProfile        = `{"profiles":[{"id":"0123456789abcdef","name":"Anna"}]}`
	// openDeadline bounds how long a test waits for Open, which must never
	// block on a file that is not regular.
	openDeadline = 5 * time.Second
)

func openProfilesFile(t *testing.T, dir string) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(openDeadline):
		require.FailNow(t, "Open blocked on profiles.json")
		return nil
	}
}

func TestAddProfile_FullStoreIsRefused(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	store.maxBytes = 0

	_, err := store.AddProfile("Anna")

	require.ErrorIs(t, err, ErrFull)
	assert.Empty(t, store.Profiles())
	assert.NoFileExists(t, profilesPath(dir))
}

func TestRenameProfile_GrowthPastTheCapIsFull(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	store.maxBytes = int64(len(fileBytes(t, profilesPath(dir))))
	before := fileBytes(t, profilesPath(dir))

	require.ErrorIs(t, store.RenameProfile(anna.ID, "Annabel"), ErrFull)
	assert.Equal(t, before, fileBytes(t, profilesPath(dir)))
	assert.Equal(t, []string{"Anna"}, profileNames(store.Profiles()))

	require.NoError(t, store.RenameProfile(anna.ID, "ANNA"), "a rename that does not grow the file")
	assert.Equal(t, []string{"ANNA"}, profileNames(store.Profiles()))
}

func TestDeleteProfile_IsAllowedWhenFull(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	store.maxBytes = 0

	require.NoError(t, store.DeleteProfile(anna.ID))

	assert.Empty(t, store.Profiles())
}

func TestOpen_RefusesAProfilesFileThatIsNotAnObjectWithAList(t *testing.T) {
	for name, content := range map[string]string{
		"misspelt key":        `{"profile":[{"id":"0123456789abcdef","name":"Anna"}]}`,
		"empty object":        `{}`,
		"null":                `null`,
		"null list":           `{"profiles":null}`,
		"an unknown key":      `{"profiles":[],"extra":1}`,
		"trailing data":       oneProfile + `x`,
		"a second value":      oneProfile + `{}`,
		"invalid UTF-8 name":  `{"profiles":[{"id":"0123456789abcdef","name":"Ann` + "\xff" + `a"}]}`,
		"an unknown profile":  `{"profiles":[{"id":"0123456789abcdef","name":"Anna","x":1}]}`,
		"a format character":  `{"profiles":[{"id":"0123456789abcdef","name":"Anna\u202e"}]}`,
		"a zero-width space":  `{"profiles":[{"id":"0123456789abcdef","name":"` + "\u200b" + `"}]}`,
		"spaces around name":  `{"profiles":[{"id":"0123456789abcdef","name":" Anna"}]}`,
		"a list, not object":  `[]`,
		"a string, not lists": `{"profiles":"Anna"}`,
	} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(profilesPath(dir), []byte(content), 0o600))

		_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))

		require.Error(t, err, name)
		assert.Contains(t, err.Error(), profilesFileName, name)
		assert.Equal(t, content, string(fileBytes(t, profilesPath(dir))), name)
	}
}

func TestOpen_LoadsAnEmptyListAndATrailingNewline(t *testing.T) {
	for _, content := range []string{`{"profiles":[]}`, oneProfile + "\n"} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(profilesPath(dir), []byte(content), 0o600))

		require.NoError(t, openProfilesFile(t, dir), content)
	}
}

func TestOpen_RefusesAProfilesPathThatIsNotARegularFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(t, os.WriteFile(target, []byte(oneProfile), 0o600))

	for name, plant := range map[string]func(path string) error{
		"a FIFO":      func(path string) error { return syscall.Mkfifo(path, 0o600) },
		"a directory": func(path string) error { return os.Mkdir(path, 0o700) },
		"a symlink":   func(path string) error { return os.Symlink(target, path) },
	} {
		dir := t.TempDir()
		require.NoError(t, plant(profilesPath(dir)), name)

		err := openProfilesFile(t, dir)

		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "not a regular file", name)
	}
}

func TestAddProfile_RefusesFormatCharacters(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)

	for _, name := range []string{"\u200b", "\u202e", "Anna\u202e", "An\u200bna", "Anna\u2028Ben"} {
		_, err := store.AddProfile(name)

		require.ErrorIs(t, err, ErrInvalid, "%q", name)
	}
	assert.Empty(t, store.Profiles())
}

// escapedNames are profileLimit distinct names of profileNameLimit bytes of
// '<' and '&', which JSON escapes to six bytes each.
func escapedNames() []string {
	const bits = 6
	found := make([]string, 0, profileLimit)
	for i := range profileLimit {
		name := strings.Repeat("<", profileNameLimit-bits)
		for bit := range bits {
			if i&(1<<bit) != 0 {
				name += "&"
				continue
			}
			name += "<"
		}
		found = append(found, name)
	}

	return found
}

func TestOpen_ReloadsTheMostProfilesWithTheLongestEscapedNames(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	for _, name := range escapedNames() {
		addProfile(t, store, name)
	}
	require.Greater(t, len(fileBytes(t, profilesPath(dir))), 16<<10)

	reopened, _ := openStore(t, dir)

	assert.Equal(t, store.Profiles(), reopened.Profiles())
	assert.Len(t, reopened.Profiles(), profileLimit)
}

func TestOpen_ProfilesFileAtTheBoundLoadsAndOneByteOverIsRefused(t *testing.T) {
	for size, wantErr := range map[int]bool{profilesFileLimit: false, profilesFileLimit + 1: true} {
		dir := t.TempDir()
		padded := oneProfile + strings.Repeat(" ", size-len(oneProfile))
		require.NoError(t, os.WriteFile(profilesPath(dir), []byte(padded), 0o600))

		err := openProfilesFile(t, dir)

		if wantErr {
			require.Error(t, err, "%d bytes", size)
			assert.Contains(t, err.Error(), "over", "%d bytes", size)
			continue
		}
		require.NoError(t, err, "%d bytes", size)
	}
}

func TestOpen_ReloadedProfilesCountTowardTheCap(t *testing.T) {
	dir := t.TempDir()
	first, _ := openStore(t, dir)
	addProfile(t, first, strings.Repeat("p", profileNameLimit))
	profileBytes := int64(len(fileBytes(t, profilesPath(dir))))
	older := save(t, first, "older")
	size := recordingBytes(t, dir, older)

	store, c := openStore(t, dir)
	// Room for the profiles and one recording: a second evicts the first
	// only because the profiles read back count.
	store.maxBytes = profileBytes + 2*size - 1
	c.advance(time.Minute)
	newer := save(t, store, "newer")

	assert.Equal(t, []string{newer.ID}, ids(t, store))
}

func TestProfiles_AFailedWriteChangesNothing(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	anna := addProfile(t, store, "Anna")
	before := fileBytes(t, profilesPath(dir))
	written := errors.New("disk gone")
	store.write = func(string, []byte) error { return written }

	_, addErr := store.AddProfile("Ben")
	renameErr := store.RenameProfile(anna.ID, "Annabel")
	deleteErr := store.DeleteProfile(anna.ID)

	for name, err := range map[string]error{"add": addErr, "rename": renameErr, "delete": deleteErr} {
		require.ErrorIs(t, err, written, name)
	}
	assert.Equal(t, []Profile{anna}, store.Profiles())
	assert.Equal(t, before, fileBytes(t, profilesPath(dir)))
}

func TestOpen_RefusesANameWithSpacesAroundIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(profilesPath(dir), []byte(`{"profiles":[{"id":"0123456789abcdef","name":"Anna "}]}`), 0o600))

	_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has spaces around it")
}

func TestOpen_RefusesAnUnreadableProfilesFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(profilesPath(dir), []byte(oneProfile), 0))

	_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))

	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), "read "+profilesFileName)
}
