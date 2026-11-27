package recordings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
