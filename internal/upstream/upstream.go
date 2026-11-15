package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Format string

const WAV Format = "wav"

type Backend string

const (
	LlamaCPP Backend = "llamacpp"
	QwenTTS  Backend = "qwentts"
)

func ParseBackend(name string) (Backend, error) {
	backend := Backend(name)
	if backend != LlamaCPP && backend != QwenTTS {
		return "", fmt.Errorf("upstream.ttsbackend must be %s or %s, got %q", LlamaCPP, QwenTTS, name)
	}

	return backend, nil
}

const (
	routePrefix        = "upstream"
	ttsPath            = "tts"
	qwenTTSPath        = "v1/tts"
	transcriptionsPath = "v1/audio/transcriptions"
	contentTypeJSON    = "application/json"
	retryAfterHeader   = "Retry-After"
	maxErrorBodyBytes  = 4096
	maxTranscriptBytes = 1 << 20
)

var errTranscriptTooLarge = fmt.Errorf("transcript is larger than %d MiB", maxTranscriptBytes>>20)

type StatusError struct {
	Code       int
	Message    string
	RetryAfter string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Code, e.Message)
}

type Client struct {
	base     *url.URL
	ttsModel string
	asrModel string
	backend  Backend
	http     *http.Client
}

type Option func(*Client)

func WithBackend(backend Backend) Option {
	return func(c *Client) { c.backend = backend }
}

func New(base, ttsModel, asrModel string, httpClient *http.Client, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("upstream url: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("upstream url %q needs a scheme and host", base)
	}

	client := &Client{base: parsed, ttsModel: ttsModel, asrModel: asrModel, backend: LlamaCPP, http: withoutRedirects(httpClient)}
	for _, apply := range opts {
		apply(client)
	}

	if _, err := ParseBackend(string(client.backend)); err != nil {
		return nil, err
	}

	return client, nil
}

func withoutRedirects(httpClient *http.Client) *http.Client {
	if httpClient == nil {
		return nil
	}

	c := *httpClient
	c.CheckRedirect = refuseRedirect

	return &c
}

// refuseRedirect stops net/http replaying a POST on 307 or 308, up to ten times per attempt.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type Speech struct {
	Voice      string
	Input      string
	Lang       string
	SpeakerRef string
	Format     Format
}

type Audio struct {
	Body    io.ReadCloser
	Status  int
	Retried bool
}

type speechRequest struct {
	path        string
	body        []byte
	contentType string
}

// Synthesize builds the request body once, before any attempt, so a retry
// sends the same bytes without reading the voice clip again.
func (c *Client) Synthesize(ctx context.Context, speech *Speech) (*Audio, error) {
	req, err := c.speechRequest(ctx, speech)
	if err != nil {
		return nil, err
	}

	resp, err := c.postSpeech(ctx, req)
	retried := retryable(ctx, err)
	if retried {
		resp, err = c.postSpeech(ctx, req)
	}

	if err != nil && retried {
		return nil, fmt.Errorf("retried once: %w", err)
	}

	if err != nil {
		return nil, err
	}

	return &Audio{Body: resp.Body, Status: resp.StatusCode, Retried: retried}, nil
}

// retryable reports a failure that happened before any audio arrived, so
// sending the same request again cannot duplicate output: a connection the
// upstream dropped while the caller still waits, or the 502 llama-swap answers
// while the model is down.
func retryable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}

	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code == http.StatusBadGateway
	}

	return true
}

func (c *Client) speechRequest(ctx context.Context, speech *Speech) (speechRequest, error) {
	if c.backend == QwenTTS {
		return qwenSpeech(speech)
	}

	return llamaSpeech(ctx, speech)
}

func (c *Client) postSpeech(ctx context.Context, req speechRequest) (*http.Response, error) {
	return c.post(ctx, c.ttsModel, req.path, bytes.NewReader(req.body), req.contentType)
}

type Recording struct {
	Name string
	Data io.Reader
}

func (c *Client) Transcribe(ctx context.Context, rec Recording) (string, error) {
	body, contentType, err := recordingForm(rec, c.asrModel)
	if err != nil {
		return "", err
	}

	resp, err := c.post(ctx, c.asrModel, transcriptionsPath, body, contentType)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return decodeTranscript(resp.Body)
}

func (c *Client) post(ctx context.Context, model, path string, body io.Reader, contentType string) (*http.Response, error) {
	target := c.base.JoinPath(routePrefix, model, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}

	return resp, nil
}

func statusError(resp *http.Response) error {
	text, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	message := errorMessage(text, resp.StatusCode)

	if err != nil {
		message = fmt.Sprintf("%s (reading the error body failed: %v)", message, err)
	}

	return &StatusError{Code: resp.StatusCode, Message: message, RetryAfter: resp.Header.Get(retryAfterHeader)}
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func errorMessage(text []byte, code int) string {
	var envelope errorEnvelope
	if json.Unmarshal(text, &envelope) == nil {
		if message := strings.TrimSpace(envelope.Error.Message); message != "" {
			return message
		}
	}

	message := strings.TrimSpace(string(text))
	if message == "" {
		return http.StatusText(code)
	}

	return message
}

type qwenSpeechBody struct {
	Voice          string `json:"voice"`
	Input          string `json:"input"`
	ResponseFormat Format `json:"response_format"`
}

func qwenSpeech(speech *Speech) (speechRequest, error) {
	body, err := json.Marshal(qwenSpeechBody{Voice: speech.Voice, Input: speech.Input, ResponseFormat: speech.Format})
	if err != nil {
		return speechRequest{}, err
	}

	return speechRequest{path: qwenTTSPath, body: body, contentType: contentTypeJSON}, nil
}

func llamaSpeech(ctx context.Context, speech *Speech) (speechRequest, error) {
	body, contentType, err := speechForm(ctx, speech)
	if err != nil {
		return speechRequest{}, err
	}

	return speechRequest{path: ttsPath, body: body, contentType: contentType}, nil
}

func speechForm(ctx context.Context, speech *Speech) (body []byte, contentType string, err error) {
	buf := &bytes.Buffer{}
	form := multipart.NewWriter(buf)

	fields := [][2]string{
		{"input", speech.Input},
		{"lang", speech.Lang},
		{"response_format", string(speech.Format)},
		{"stream", strconv.FormatBool(true)},
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, "", err
		}
	}

	if err := attachFile(ctx, form, "speaker_ref", speech.SpeakerRef); err != nil {
		return nil, "", err
	}

	if err := form.Close(); err != nil {
		return nil, "", err
	}

	return buf.Bytes(), form.FormDataContentType(), nil
}

func attachFile(ctx context.Context, form *multipart.Writer, field, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("speaker reference: %w", err)
	}
	defer file.Close()

	part, err := form.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}

	_, err = io.Copy(part, &contextReader{ctx: ctx, src: file})
	return err
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.src.Read(p)
}

func recordingForm(rec Recording, model string) (io.Reader, string, error) {
	buf := &bytes.Buffer{}
	form := multipart.NewWriter(buf)

	part, err := form.CreateFormFile("file", rec.Name)
	if err != nil {
		return nil, "", err
	}

	if _, err := io.Copy(part, rec.Data); err != nil {
		return nil, "", err
	}

	if err := form.WriteField("model", model); err != nil {
		return nil, "", err
	}

	if err := form.Close(); err != nil {
		return nil, "", err
	}

	return buf, form.FormDataContentType(), nil
}

type transcript struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

func decodeTranscript(body io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxTranscriptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}

	if len(data) > maxTranscriptBytes {
		return "", errTranscriptTooLarge
	}

	var result transcript
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("decode transcript: %w", err)
	}

	if result.Text == nil {
		return "", fmt.Errorf("transcript of type %q has no text", result.Type)
	}

	return *result.Text, nil
}
