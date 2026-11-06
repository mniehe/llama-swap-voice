package upstream

import (
	"bytes"
	"context"
	"encoding/json"
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

const (
	routePrefix        = "upstream"
	ttsPath            = "tts"
	transcriptionsPath = "v1/audio/transcriptions"
	maxErrorBodyBytes  = 4096
	maxTranscriptBytes = 1 << 20
)

var errTranscriptTooLarge = fmt.Errorf("transcript is larger than %d MiB", maxTranscriptBytes>>20)

type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Code, e.Message)
}

type Client struct {
	base     *url.URL
	ttsModel string
	asrModel string
	http     *http.Client
}

func New(base, ttsModel, asrModel string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("upstream url: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("upstream url %q needs a scheme and host", base)
	}

	return &Client{base: parsed, ttsModel: ttsModel, asrModel: asrModel, http: httpClient}, nil
}

type Speech struct {
	Input      string
	Lang       string
	SpeakerRef string
	Format     Format
}

type Audio struct {
	Body   io.ReadCloser
	Status int
}

func (c *Client) Synthesize(ctx context.Context, speech Speech) (*Audio, error) {
	body, contentType, err := speechForm(ctx, speech)
	if err != nil {
		return nil, err
	}

	resp, err := c.post(ctx, c.ttsModel, ttsPath, body, contentType)
	if err != nil {
		return nil, err
	}

	return &Audio{Body: resp.Body, Status: resp.StatusCode}, nil
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
	message := strings.TrimSpace(string(text))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}

	if err != nil {
		message = fmt.Sprintf("%s (reading the error body failed: %v)", message, err)
	}

	return &StatusError{Code: resp.StatusCode, Message: message}
}

func speechForm(ctx context.Context, speech Speech) (io.Reader, string, error) {
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

	return buf, form.FormDataContentType(), nil
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
