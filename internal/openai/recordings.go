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
	"net"
	"net/http"
	"net/url"
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
	// maxKeepFormBytes bounds the keep form, whose one field fits in a few
	// bytes.
	maxKeepFormBytes = 1 << 10
	// maxRecordingFormBytes bounds the recording form. Percent-encoding
	// writes a byte as at most three, so the correct text at its bound takes
	// three times MaxCorrectTextBytes; a KiB covers the field names and the
	// two checkboxes.
	maxRecordingFormBytes = 3*recordings.MaxCorrectTextBytes + 1<<10
	contentTypeHTML       = "text/html; charset=utf-8"
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
)

var pageFuncs = template.FuncMap{
	"retentionDays": func() int { return int(recordings.Retention.Hours()) / hoursPerDay },
}

var (
	recordingsPage = template.Must(template.New("recordings").Funcs(pageFuncs).Parse(recordingsHTML))
	recordingPage  = template.Must(template.New("recording").Funcs(pageFuncs).Parse(recordingHTML))
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
}

// recordingView is what the page of one recording renders: its row, with
// the whole transcript, and the form's correct text.
type recordingView struct {
	recordingRow
	CorrectText string
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
		return s.allowedHost(protection.Handler(next).ServeHTTP)
	}
	recording := recordingsRoute + "/{" + recordingIDKey + "}"

	return []route{
		{http.MethodGet, recordingsRoute, s.allowedHost(s.handleRecordings)},
		{http.MethodGet, recording, s.allowedHost(s.handleRecording)},
		{http.MethodPost, recording, protect(s.handleRecordingEdit)},
		{http.MethodGet, recording + "/audio", notStored(s.allowedHost(s.handleRecordingAudio))},
		{http.MethodPost, recording + "/keep", protect(s.handleRecordingKeep)},
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

	view := recordingView{recordingRow: newRecordingRow(&rec), CorrectText: rec.CorrectText}
	s.writePage(w, recordingPage, &view)
}

// handleRecordingEdit saves the recording form and sends the browser back to
// the recording's page.
func (s *Server) handleRecordingEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue(recordingIDKey)
	if !recordings.ValidID(id) {
		s.recordingFailure(w, r, recordings.ErrNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRecordingFormBytes)
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

	return recordings.Annotation{Keep: keep, TranscriptionError: flagged, CorrectText: text}, nil
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
	}
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
// that conflicts with what it holds, and 500 for anything else, which only
// the log details.
func (s *Server) recordingFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, recordings.ErrNotFound):
		s.writeError(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
		return
	case errors.Is(err, recordings.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err)
		return
	case errors.Is(err, recordings.ErrFlaggedKept):
		s.writeError(w, http.StatusConflict, err)
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
