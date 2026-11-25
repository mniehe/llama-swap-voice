package openai

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	// maxKeepFormBytes bounds the keep form, whose one field fits in a few
	// bytes.
	maxKeepFormBytes = 1 << 10
	contentTypeHTML  = "text/html; charset=utf-8"
	shownTime        = "2006-01-02 15:04:05"
	hoursPerDay      = 24
)

//go:embed recordings.html
var recordingsHTML string

var recordingsPage = template.Must(template.New("recordings").Funcs(template.FuncMap{
	"retentionDays": func() int { return int(recordings.Retention.Hours()) / hoursPerDay },
}).Parse(recordingsHTML))

// recordingRow is one recording as the page shows it.
type recordingRow struct {
	ID         string
	At         string
	Shown      string
	Length     string
	Source     string
	Transcript string
	Keep       bool
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
// from store.
func WithRecordings(store *recordings.Store) Option {
	return func(s *Server) {
		s.recordings = store
	}
}

func (s *Server) recordingRoutes() []route {
	if s.recordings == nil {
		return nil
	}

	return []route{
		{http.MethodGet, recordingsRoute, s.handleRecordings},
		{http.MethodGet, recordingsRoute + "/{" + recordingIDKey + "}/audio", s.handleRecordingAudio},
		{http.MethodPost, recordingsRoute + "/{" + recordingIDKey + "}/keep", s.handleRecordingKeep},
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

	var page bytes.Buffer
	if err := recordingsPage.Execute(&page, &view); err != nil {
		s.log.Error("could not render recordings page", slog.Any("error", err))
		s.writeError(w, http.StatusInternalServerError, errors.New("could not render recordings page"))
		return
	}

	w.Header().Set("Content-Type", contentTypeHTML)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := page.WriteTo(w); err != nil {
		s.logWriteFailure(err)
	}
}

// pageCursor returns the before parameter, empty for the newest page.
func pageCursor(r *http.Request) (string, error) {
	values, present := r.URL.Query()[beforeParam]
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
		ID:         rec.ID,
		At:         rec.At.UTC().Format(time.RFC3339),
		Shown:      rec.At.UTC().Format(shownTime),
		Length:     fmt.Sprintf("%.1f s", rec.Duration.Seconds()),
		Source:     rec.Source,
		Transcript: rec.Transcript,
		Keep:       rec.Keep,
	}
}

func (s *Server) handleRecordingAudio(w http.ResponseWriter, r *http.Request) {
	file, err := s.recordings.Audio(r.PathValue(recordingIDKey))
	if err != nil {
		s.recordingFailure(w, r, err)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentTypeOgg)
	if info, statErr := file.Stat(); statErr == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, file); err != nil {
		s.logWriteFailure(err)
	}
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

	var keep bool
	switch value := r.PostFormValue(keepFieldName); value {
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

func (s *Server) recordingFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, recordings.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
		return
	}

	s.log.Error("recording request failed", slog.String("request_id", record(r).id), slog.Any("error", err))
	s.writeError(w, http.StatusInternalServerError, errors.New("could not read the recording"))
}
