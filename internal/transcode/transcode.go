package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Format string

const (
	MP3  Format = "mp3"
	Opus Format = "opus"
	FLAC Format = "flac"
	AAC  Format = "aac"
	WAV  Format = "wav"
	PCM  Format = "pcm"
)

const (
	stdinPipe     = "pipe:0"
	stdoutPipe    = "pipe:1"
	asrSampleRate = 16000
	asrChannels   = 1
	asrSampleSize = 2
	maxASRSeconds = 20 * 60
	wavHeaderSize = 44
	maxASRBytes   = maxASRSeconds*asrSampleRate*asrChannels*asrSampleSize + wavHeaderSize
	minAtempo     = 0.5
	killDelay     = time.Second
	maxStderr     = 4096
	spoolPattern  = "llama-swap-voice-asr-*"

	asrInputFormats = "ogg,mp3,flac,wav,mov,matroska,webm,aac"

	pcmSampleRate = 24000
	pcmChannels   = 1
)

type codec struct {
	muxer      string
	encoder    string
	sampleRate int
	channels   int
}

var codecs = map[Format]codec{
	MP3:  {muxer: "mp3", encoder: "libmp3lame"},
	Opus: {muxer: "ogg", encoder: "libopus"},
	FLAC: {muxer: "flac", encoder: "flac"},
	AAC:  {muxer: "adts", encoder: "aac"},
	WAV:  {muxer: "wav", encoder: "pcm_s16le"},
	PCM:  {muxer: "s16le", encoder: "pcm_s16le", sampleRate: pcmSampleRate, channels: pcmChannels},
}

var ErrTooLong = fmt.Errorf("audio is longer than %d minutes", maxASRSeconds/60)

type Encoder struct {
	ffmpeg      string
	maxASRBytes int
}

func New(ffmpeg string) *Encoder {
	return &Encoder{ffmpeg: ffmpeg, maxASRBytes: maxASRBytes}
}

type Options struct {
	Format Format
	Speed  float64
}

func EncodeArgs(opts Options) ([]string, error) {
	out, ok := codecs[opts.Format]
	if !ok {
		return nil, fmt.Errorf("unsupported format %q", opts.Format)
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-f", "wav", "-i", stdinPipe}
	if filter := atempo(opts.Speed); filter != "" {
		args = append(args, "-filter:a", filter)
	}

	args = append(args, out.resampling()...)
	return append(args, "-f", out.muxer, "-c:a", out.encoder, stdoutPipe), nil
}

// Raw PCM carries no header, so its rate and layout must be fixed rather
// than inherited from whatever the upstream WAV declares.
func (c codec) resampling() []string {
	if c.sampleRate == 0 {
		return nil
	}

	return []string{"-ar", strconv.Itoa(c.sampleRate), "-ac", strconv.Itoa(c.channels)}
}

func ASRArgs(input string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-xerror",
		"-format_whitelist", asrInputFormats, "-i", input,
		"-ar", strconv.Itoa(asrSampleRate), "-ac", strconv.Itoa(asrChannels),
		"-f", "wav", "-c:a", "pcm_s16le", stdoutPipe,
	}
}

func atempo(speed float64) string {
	if speed <= 0 || speed == 1 {
		return ""
	}

	var stages []string
	for speed < minAtempo {
		stages = append(stages, formatTempo(minAtempo))
		speed /= minAtempo
	}

	return strings.Join(append(stages, formatTempo(speed)), ",")
}

func formatTempo(value float64) string {
	return "atempo=" + strconv.FormatFloat(value, 'f', -1, 64)
}

func (e *Encoder) Encode(ctx context.Context, src io.Reader, opts Options) (io.ReadCloser, error) {
	args, err := EncodeArgs(opts)
	if err != nil {
		return nil, err
	}

	cmd := e.command(ctx, src, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	return &stream{cmd: cmd, stdout: stdout}, nil
}

func (e *Encoder) ToASRInput(ctx context.Context, src io.Reader) ([]byte, error) {
	input, err := spool(src)
	if err != nil {
		return nil, err
	}
	defer os.Remove(input)

	return e.decodeASR(ctx, input)
}

// Containers such as MP4 with the moov atom at the end need a seekable input;
// from a pipe ffmpeg decodes nothing and still exits 0.
func spool(src io.Reader) (string, error) {
	file, err := os.CreateTemp("", spoolPattern)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if _, err := io.Copy(file, src); err != nil {
		os.Remove(file.Name())
		return "", err
	}

	return file.Name(), nil
}

func (e *Encoder) decodeASR(ctx context.Context, input string) ([]byte, error) {
	cmd := e.command(ctx, nil, ASRArgs(input))
	out := &cappedBuffer{limit: e.maxASRBytes}
	cmd.Stdout = out

	err := cmd.Run()
	if out.overflowed {
		return nil, ErrTooLong
	}

	if err != nil {
		return nil, ffmpegError(cmd, err)
	}

	return out.Bytes(), nil
}

func (e *Encoder) command(ctx context.Context, src io.Reader, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, e.ffmpeg, args...) //nolint:gosec // G204: the binary is operator configuration and every argument is built from fixed flags.
	cmd.Stdin = src
	cmd.Stderr = &limitedBuffer{limit: maxStderr}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = killDelay
	return cmd
}

type stream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
}

func (s *stream) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

func (s *stream) Close() error {
	s.stdout.Close()
	if err := s.cmd.Wait(); err != nil {
		return ffmpegError(s.cmd, err)
	}

	return nil
}

func ffmpegError(cmd *exec.Cmd, err error) error {
	stderr, ok := cmd.Stderr.(*limitedBuffer)
	if !ok || stderr.Len() == 0 {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.Len()
	if room <= 0 {
		return len(p), nil
	}

	if _, err := b.Buffer.Write(p[:min(room, len(p))]); err != nil {
		return 0, err
	}

	return len(p), nil
}

type cappedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.overflowed = true
		return 0, ErrTooLong
	}

	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}
