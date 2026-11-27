package openai

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/recordings"
)

const (
	recordingsRoute = "/recordings"
	recordingIDKey  = "id"
	beforeParam     = "before"
	// pageSize is how many recordings one page shows, newest first.
	pageSize      = 100
	keepFieldName = "keep"
	keepOn        = "true"
	keepOff       = "false"
	// errorFieldName and correctTextFieldName name the recording form's
	// fields besides keep. A checkbox is sent with keepOn when ticked and not
	// at all when not.
	errorFieldName       = "transcriptionError"
	correctTextFieldName = "correctText"
	// profileFieldName is given once for each speaker ticked on the
	// recording form.
	profileFieldName = "profile"
	// nameFieldName names the one field of the profile forms.
	nameFieldName = "name"
	// profilesSegment is the path segment of the profiles page, which no
	// recording id can be.
	profilesSegment = "profiles"
	profileIDKey    = "profile"
	// maxKeepFormBytes bounds the keep form, whose one field fits in a few
	// bytes.
	maxKeepFormBytes = 1 << 10
	// maxRecordingFormBytes bounds the recording form. A browser sends each
	// line break of a textarea as CRLF, percent-encoded as six bytes, and
	// the handler stores it as one LF, so the correct text at its bound takes
	// at most six times MaxCorrectTextBytes; any other byte takes at most
	// three. Each of the MaxProfiles speakers takes its field name and id,
	// and a KiB covers the other field names and the two checkboxes.
	maxRecordingFormBytes = len("%0D%0A")*recordings.MaxCorrectTextBytes +
		recordings.MaxProfiles*(len("&"+profileFieldName+"=")+recordings.ProfileIDLength) + 1<<10
	// maxProfileFormBytes bounds a profile form: its one name, at the
	// store's 64-byte bound and percent-encoded, takes 192 bytes.
	maxProfileFormBytes = 1 << 10
	contentTypeHTML     = "text/html; charset=utf-8"
	// formMediaType is the one media type the recordings forms accept.
	formMediaType = "application/x-www-form-urlencoded"
	// minPlaybackBytesPerSecond is the slowest a client may fetch a
	// recording: 32 kbps Opus plays at 4 KiB/s, so this leaves a margin of
	// four.
	minPlaybackBytesPerSecond = 16 << 10
	// maxPlaybackSeconds keeps the playback deadline from overflowing
	// time.Duration for an absurd file size.
	maxPlaybackSeconds = int64(math.MaxInt64 / time.Second)
	shownTime          = "2006-01-02 15:04:05"
	hoursPerDay        = 24
	// framePolicy keeps another site from framing the page and its keep
	// buttons.
	framePolicy = "frame-ancestors 'none'"
	// maxFailureDetailBytes bounds the error text kept from ServeContent.
	maxFailureDetailBytes = 1 << 10
)

// servedHeaders are the headers http.ServeContent sets for the audio, which
// a JSON error must not carry.
var servedHeaders = []string{"Content-Type", "Content-Length", "Content-Encoding", "X-Content-Type-Options", "Last-Modified", "Etag"}

var (
	//go:embed recordings.html
	recordingsHTML string
	//go:embed recording.html
	recordingHTML string
	//go:embed profiles.html
	profilesHTML string
)

var pageFuncs = template.FuncMap{
	"retentionDays": func() int { return int(recordings.Retention.Hours()) / hoursPerDay },
}

var (
	recordingsPage = template.Must(template.New("recordings").Funcs(pageFuncs).Parse(recordingsHTML))
	recordingPage  = template.Must(template.New("recording").Funcs(pageFuncs).Parse(recordingHTML))
	profilesPage   = template.Must(template.New("profiles").Funcs(pageFuncs).Parse(profilesHTML))
)

// recordingRow is one recording as the page shows it.
type recordingRow struct {
	ID                 string
	At                 string
	Shown              string
	Length             string
	Source             string
	Transcript         string
	Keep               bool
	TranscriptionError bool
	// Speakers holds the names of the recording's profiles, joined.
	Speakers string
}

// recordingView is what the page of one recording renders: its row, with
// the whole transcript, the form's correct text and a choice for each
// profile.
type recordingView struct {
	recordingRow
	CorrectText string
	Profiles    []profileChoice
}

// profileChoice is one profile's checkbox on the recording form.
type profileChoice struct {
	ID      string
	Name    string
	Checked bool
}

// profilesView is what the profiles page renders.
type profilesView struct {
	Profiles     []recordings.Profile
	MaxProfiles  int
	MaxNameBytes int
}

// recordingsView is what the page template renders. Older is the id that
// the link to the next older page starts before, empty on the last page.
type recordingsView struct {
	Rows  []recordingRow
	Total int
	Older string
}

// Option changes how New builds a Server.
type Option func(*Server)

// WithRecordings serves the recordings page and its audio and keep routes
// from store, to requests whose Host header names loopback or one of hosts.
// An entry of hosts is a bare host, matching any port, or a host:port.
func WithRecordings(store *recordings.Store, hosts []string) Option {
	return func(s *Server) {
		s.recordings = store
		s.recordingHosts = newHostAllowList(hosts)
	}
}

func (s *Server) recordingRoutes() []route {
	if s.recordings == nil {
		return nil
	}

	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.writeError(w, http.StatusForbidden, errors.New("cross-origin request refused"))
	}))
	protect := func(next http.HandlerFunc) http.HandlerFunc {
		return s.allowedHost(protection.Handler(s.formOnly(next)).ServeHTTP)
	}
	recording := recordingsRoute + "/{" + recordingIDKey + "}"
	profile := recordingsRoute + "/" + profilesSegment + "/{" + profileIDKey + "}"

	// The profiles page shares the pattern of a recording's page: the 405
	// pattern Handler adds for each path would conflict with a pattern of its
	// own.
	return []route{
		{http.MethodGet, recordingsRoute, s.allowedHost(s.handleRecordings)},
		{http.MethodGet, recording, s.allowedHost(orProfiles(s.handleRecording, s.handleProfiles))},
		{http.MethodPost, recording, protect(orProfiles(s.handleRecordingEdit, s.handleProfileAdd))},
		{http.MethodGet, recording + "/audio", notStored(s.allowedHost(s.handleRecordingAudio))},
		{http.MethodPost, recording + "/keep", protect(s.handleRecordingKeep)},
		{http.MethodPost, profile + "/rename", protect(s.handleProfileRename)},
		{http.MethodPost, profile + "/delete", protect(s.handleProfileDelete)},
	}
}

// orProfiles routes a request for /recordings/profiles to profiles and any
// other to recording.
func orProfiles(recording, profiles http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue(recordingIDKey) == profilesSegment {
			profiles(w, r)
			return
		}

		recording(w, r)
	}
}

// formOnly answers 415 unless the request's body is a URL-encoded form.
// http.Request.ParseForm reads no body of another type and leaves the form
// empty, which the recording form would save as a review with every field
// cleared, so the type is checked before any handler reads the body.
func (s *Server) formOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != formMediaType {
			s.writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("a recordings form must be sent as %s", formMediaType))
			return
		}

		next(w, r)
	}
}

// allowedHost answers 403 unless the request's Host header is on the
// recordings allow-list.
func (s *Server) allowedHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.recordingHosts.allows(r.Host) {
			s.writeError(w, http.StatusForbidden, fmt.Errorf("host %q may not reach the recordings", r.Host))
			return
		}

		next(w, r)
	}
}

// notStored marks every reply of next, an error included, as one no cache
// may keep.
func notStored(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// The page is rendered whole before it is sent, so a template failure is a
// JSON 500 rather than half a page.
func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	before, err := pageCursor(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	// One row past the page tells whether an older page exists.
	list, total, err := s.recordings.List(before, pageSize+1)
	if err != nil {
		s.log.Error("could not list recordings", slog.String("request_id", record(r).id), slog.Any("error", err))
		s.writeError(w, http.StatusInternalServerError, errors.New("could not list recordings"))
		return
	}

	view := recordingsView{Total: total}
	if len(list) > pageSize {
		list = list[:pageSize]
		view.Older = list[pageSize-1].ID
	}
	view.Rows = make([]recordingRow, 0, len(list))
	for i := range list {
		view.Rows = append(view.Rows, newRecordingRow(&list[i]))
	}

	s.writePage(w, recordingsPage, &view)
}

// writePage renders page whole before it sends it, so a template failure is
// a JSON 500 rather than half a page. No cache keeps it and no other site
// frames it.
func (s *Server) writePage(w http.ResponseWriter, page *template.Template, view any) {
	var rendered bytes.Buffer
	if err := page.Execute(&rendered, view); err != nil {
		s.log.Error("could not render page", slog.String("page", page.Name()), slog.Any("error", err))
		s.writeError(w, http.StatusInternalServerError, fmt.Errorf("could not render %s page", page.Name()))
		return
	}

	w.Header().Set("Content-Type", contentTypeHTML)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", framePolicy)
	w.WriteHeader(http.StatusOK)
	if _, err := rendered.WriteTo(w); err != nil {
		s.logWriteFailure(err)
	}
}

// handleRecording shows one recording with its whole transcript and the
// form that edits it.
func (s *Server) handleRecording(w http.ResponseWriter, r *http.Request) {
	rec, err := s.recordings.Recording(r.PathValue(recordingIDKey))
	if err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	view := recordingView{
		recordingRow: newRecordingRow(&rec),
		CorrectText:  rec.CorrectText,
		Profiles:     profileChoices(s.recordings.Profiles(), rec.Profiles),
	}
	s.writePage(w, recordingPage, &view)
}

// profileChoices offers every profile, ticking those in assigned. It matches
// by id alone: the two lists are read apart, and a rename between the reads
// must not untick a speaker.
func profileChoices(all, assigned []recordings.Profile) []profileChoice {
	choices := make([]profileChoice, 0, len(all))
	for _, profile := range all {
		choices = append(choices, profileChoice{
			ID:   profile.ID,
			Name: profile.Name,
			Checked: slices.ContainsFunc(assigned, func(p recordings.Profile) bool {
				return p.ID == profile.ID
			}),
		})
	}

	return choices
}

// handleRecordingEdit saves the recording form and sends the browser back to
// the recording's page.
func (s *Server) handleRecordingEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue(recordingIDKey)
	if !recordings.ValidID(id) {
		s.recordingFailure(w, r, recordings.ErrNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(maxRecordingFormBytes))
	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid recording form: %w", err))
		return
	}

	edit, err := annotationOf(r.PostForm)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.recordings.Annotate(id, edit); err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	http.Redirect(w, r, recordingsRoute+"/"+id, http.StatusSeeOther)
}

// annotationOf reads the recording form. A checkbox field is absent or
// keepOn, and no field is given twice.
func annotationOf(form url.Values) (recordings.Annotation, error) {
	keep, keepErr := checkbox(form, keepFieldName)
	flagged, flagErr := checkbox(form, errorFieldName)
	text, textErr := singleField(form, correctTextFieldName)
	if err := errors.Join(keepErr, flagErr, textErr); err != nil {
		return recordings.Annotation{}, err
	}

	return recordings.Annotation{
		Keep:               keep,
		TranscriptionError: flagged,
		CorrectText:        lineFeeds(text),
		Profiles:           form[profileFieldName],
	}, nil
}

// lineFeeds turns each CRLF and lone CR in text into LF. A browser sends a
// textarea's line breaks as CRLF, and the store keeps and bounds them as LF.
func lineFeeds(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func checkbox(form url.Values, name string) (bool, error) {
	if !form.Has(name) {
		return false, nil
	}

	value, err := singleField(form, name)
	if err != nil {
		return false, err
	}

	if value != keepOn {
		return false, fmt.Errorf("%s must be %s when given, got %q", name, keepOn, value)
	}

	return true, nil
}

// singleField returns the value of name, empty when it is absent, and fails
// when it is given more than once.
func singleField(form url.Values, name string) (string, error) {
	values := form[name]
	if len(values) > 1 {
		return "", fmt.Errorf("give %s at most once, got %d values", name, len(values))
	}

	if len(values) == 0 {
		return "", nil
	}

	return values[0], nil
}

// pageCursor returns the before parameter, empty for the newest page. The
// query is parsed strictly, so a malformed escape is refused rather than
// dropped.
func pageCursor(r *http.Request) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid query: %w", err)
	}

	values, present := query[beforeParam]
	if !present {
		return "", nil
	}

	if len(values) != 1 || !recordings.ValidID(values[0]) {
		return "", fmt.Errorf("%s must be one recording id, got %q", beforeParam, values)
	}

	return values[0], nil
}

func newRecordingRow(rec *recordings.Recording) recordingRow {
	return recordingRow{
		ID:                 rec.ID,
		At:                 rec.At.UTC().Format(time.RFC3339),
		Shown:              rec.At.UTC().Format(shownTime),
		Length:             fmt.Sprintf("%.1f s", rec.Duration.Seconds()),
		Source:             rec.Source,
		Transcript:         rec.Transcript,
		Keep:               rec.Keep,
		TranscriptionError: rec.TranscriptionError,
		Speakers:           speakerNames(rec.Profiles),
	}
}

func speakerNames(profiles []recordings.Profile) string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}

	return strings.Join(names, ", ")
}

// handleProfiles shows the profiles with the forms that add, rename and
// delete them.
func (s *Server) handleProfiles(w http.ResponseWriter, _ *http.Request) {
	view := profilesView{Profiles: s.recordings.Profiles(), MaxProfiles: recordings.MaxProfiles, MaxNameBytes: recordings.MaxProfileNameBytes}
	s.writePage(w, profilesPage, &view)
}

func (s *Server) handleProfileAdd(w http.ResponseWriter, r *http.Request) {
	name, err := profileName(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	if _, err := s.recordings.AddProfile(name); err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	http.Redirect(w, r, recordingsRoute+"/"+profilesSegment, http.StatusSeeOther)
}

func (s *Server) handleProfileRename(w http.ResponseWriter, r *http.Request) {
	name, err := profileName(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.recordings.RenameProfile(r.PathValue(profileIDKey), name); err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	http.Redirect(w, r, recordingsRoute+"/"+profilesSegment, http.StatusSeeOther)
}

// handleProfileDelete deletes a profile. Its form has no fields, but its
// body is still read through the bound.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProfileFormBytes)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid profile form: %w", err))
		return
	}

	if err := s.recordings.DeleteProfile(r.PathValue(profileIDKey)); err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	http.Redirect(w, r, recordingsRoute+"/"+profilesSegment, http.StatusSeeOther)
}

// profileName reads the name field, given exactly once, from a profile form
// bounded by maxProfileFormBytes. The store checks the name itself.
func profileName(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProfileFormBytes)
	if err := r.ParseForm(); err != nil {
		return "", fmt.Errorf("invalid profile form: %w", err)
	}

	values := r.PostForm[nameFieldName]
	if len(values) != 1 {
		return "", fmt.Errorf("give %s exactly once, got %d values", nameFieldName, len(values))
	}

	return values[0], nil
}

// handleRecordingAudio serves the file with its own deadline, long enough to
// send the whole file at minPlaybackBytesPerSecond, because a browser reads
// audio about as fast as it plays it. It takes no ASR or TTS slot.
func (s *Server) handleRecordingAudio(w http.ResponseWriter, r *http.Request) {
	file, err := s.recordings.Audio(r.PathValue(recordingIDKey))
	if err != nil {
		s.recordingFailure(w, r, err)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		s.recordingFailure(w, r, fmt.Errorf("stat recording: %w", err))
		return
	}

	deadline := time.Now().Add(s.playbackTimeout(info.Size()))
	s.setDeadlines(w, deadline, deadline)

	s.serveRecording(w, r, info.ModTime(), file)
}

// serveRecording serves content with ranges and conditional requests. An
// error http.ServeContent would answer in plain text is answered as JSON.
func (s *Server) serveRecording(w http.ResponseWriter, r *http.Request, modified time.Time, content io.ReadSeeker) {
	w.Header().Set("Content-Type", contentTypeOgg)
	w.Header().Set("Cache-Control", "no-store")

	held := &errorHoldingWriter{ResponseWriter: w}
	http.ServeContent(held, r, "", modified, content)

	if held.status != 0 {
		s.contentFailure(w, r, held.status, string(held.detail))
	}
}

// contentFailure answers status, which http.ServeContent chose, through
// writeError. Only a 416 keeps the Content-Range that names the file's size.
func (s *Server) contentFailure(w http.ResponseWriter, r *http.Request, status int, detail string) {
	header := w.Header()
	for _, name := range servedHeaders {
		header.Del(name)
	}
	if status != http.StatusRequestedRangeNotSatisfiable {
		header.Del("Content-Range")
	}
	header.Set("Cache-Control", "no-store")

	if status >= http.StatusInternalServerError {
		s.recordingFailure(w, r, fmt.Errorf("serve recording: %s", strings.TrimSpace(detail)))
		return
	}

	s.writeError(w, status, errors.New(strings.ToLower(http.StatusText(status))))
}

// errorHoldingWriter passes a response through unless its status is an
// error, which it holds back with a bounded copy of its body, so that the
// handler can answer it instead. http.ServeContent writes its own 412 with no
// body and its 416 and 500 in plain text.
type errorHoldingWriter struct {
	http.ResponseWriter
	status  int
	started bool
	detail  []byte
}

func (e *errorHoldingWriter) WriteHeader(code int) {
	if !e.started && code >= http.StatusBadRequest {
		e.status = code
		return
	}

	e.started = true
	e.ResponseWriter.WriteHeader(code)
}

func (e *errorHoldingWriter) Write(p []byte) (int, error) {
	if e.status != 0 {
		room := max(0, maxFailureDetailBytes-len(e.detail))
		e.detail = append(e.detail, p[:min(len(p), room)]...)
		return len(p), nil
	}

	e.started = true
	return e.ResponseWriter.Write(p)
}

func (e *errorHoldingWriter) Unwrap() http.ResponseWriter {
	return e.ResponseWriter
}

// playbackTimeout is the time to send size bytes at
// minPlaybackBytesPerSecond, and never less than the control timeout.
func (s *Server) playbackTimeout(size int64) time.Duration {
	seconds := min(max(size, 0)/minPlaybackBytesPerSecond+1, maxPlaybackSeconds)

	return max(s.controlTimeout, time.Duration(seconds)*time.Second)
}

func (s *Server) handleRecordingKeep(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue(recordingIDKey)
	if !recordings.ValidID(id) {
		s.recordingFailure(w, r, recordings.ErrNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxKeepFormBytes)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid keep form: %w", err))
		return
	}

	values := r.PostForm[keepFieldName]
	if len(values) != 1 {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("give %s exactly once, got %d values", keepFieldName, len(values)))
		return
	}

	var keep bool
	switch value := values[0]; value {
	case keepOn:
		keep = true
	case keepOff:
		keep = false
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("keep must be %s or %s, got %q", keepOn, keepOff, value))
		return
	}

	if err := s.recordings.SetKeep(id, keep); err != nil {
		s.recordingFailure(w, r, err)
		return
	}

	http.Redirect(w, r, recordingsRoute, http.StatusSeeOther)
}

// recordingFailure answers err from the store: 404 for a recording or
// profile it does not hold, 400 for a value it refuses, 409 for a change
// that conflicts with what it holds, 507 for an edit that would pass the
// store's cap, and 500 for anything else, which only the log details.
func (s *Server) recordingFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, recordings.ErrNotFound):
		s.writeError(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
		return
	case errors.Is(err, recordings.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err)
		return
	case errors.Is(err, recordings.ErrFlaggedKept), errors.Is(err, recordings.ErrDuplicateName):
		s.writeError(w, http.StatusConflict, err)
		return
	case errors.Is(err, recordings.ErrFull):
		s.writeError(w, http.StatusInsufficientStorage, err)
		return
	}

	s.log.Error("recording request failed", slog.String("request_id", record(r).id), slog.Any("error", err))
	s.writeError(w, http.StatusInternalServerError, errors.New("could not read the recording"))
}

// loopbackHosts reach the recordings routes whatever the configuration.
var loopbackHosts = []string{"127.0.0.1", "::1", "localhost"}

// hostAllowList holds the authorities a recordings request may name in its
// Host header. A DNS-rebinding page reaches the service under its own name,
// so the Host header, never X-Forwarded-Host, decides.
type hostAllowList struct {
	// anyPort holds hosts that match on every port, without brackets.
	anyPort map[string]bool
	// exact holds host:port authorities that match only on that port.
	exact map[string]bool
}

// newHostAllowList takes entries that are a bare host, which matches on any
// port, or a host:port, which matches only that authority.
func newHostAllowList(entries []string) *hostAllowList {
	list := &hostAllowList{anyPort: map[string]bool{}, exact: map[string]bool{}}
	for _, entry := range append(append([]string(nil), loopbackHosts...), entries...) {
		list.add(entry)
	}

	return list
}

func (l *hostAllowList) add(entry string) {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "" {
		return
	}

	if host, port, err := net.SplitHostPort(entry); err == nil {
		l.exact[net.JoinHostPort(host, port)] = true
		return
	}

	l.anyPort[strings.Trim(entry, "[]")] = true
}

func (l *hostAllowList) allows(authority string) bool {
	authority = strings.ToLower(authority)

	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), ""
	}

	if host != "" && l.anyPort[host] {
		return true
	}

	return port != "" && l.exact[net.JoinHostPort(host, port)]
}
