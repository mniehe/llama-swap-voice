package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stdinPipe  = "pipe:0"
	stdoutPipe = "pipe:1"
	minAtempo  = 0.5
	maxStderr  = 4096

	rawPCM        = "s16le"
	pcmSampleRate = 24000
	pcmChannels   = 1
)

// KillDelay is how long ffmpeg has to exit after SIGTERM before it is killed.
const KillDelay = time.Second

type Encoder struct {
	ffmpeg string
}

func New(ffmpeg string) *Encoder {
	return &Encoder{ffmpeg: ffmpeg}
}

type Options struct {
	Speed float64
}

// EncodeArgs stretches raw 24 kHz mono s16le samples by the requested speed
// and writes them back in the same layout.
func EncodeArgs(opts Options) []string {
	pcm := []string{"-f", rawPCM, "-ar", strconv.Itoa(pcmSampleRate), "-ac", strconv.Itoa(pcmChannels)}

	args := append([]string{"-hide_banner", "-loglevel", "error"}, pcm...)
	args = append(args, "-i", stdinPipe)
	if filter := atempo(opts.Speed); filter != "" {
		args = append(args, "-filter:a", filter)
	}

	args = append(args, pcm...)
	return append(args, stdoutPipe)
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
	cmd := e.command(ctx, src, EncodeArgs(opts))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	return &stream{cmd: cmd, stdout: stdout}, nil
}

func (e *Encoder) command(ctx context.Context, src io.Reader, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, e.ffmpeg, args...) //nolint:gosec // G204: the binary is operator configuration and every argument is built from fixed flags.
	cmd.Stdin = src
	cmd.Stderr = &limitedBuffer{limit: maxStderr}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = KillDelay
	return cmd
}

type stream struct {
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	once     sync.Once
	closeErr error
}

func (s *stream) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

// Close waits for ffmpeg once; later calls return the same result.
func (s *stream) Close() error {
	s.once.Do(func() {
		s.stdout.Close()
		if err := s.cmd.Wait(); err != nil {
			s.closeErr = ffmpegError(s.cmd, err)
		}
	})

	return s.closeErr
}

func ffmpegError(cmd *exec.Cmd, err error) error {
	stderr, ok := cmd.Stderr.(*limitedBuffer)
	if !ok || stderr.Len() == 0 {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// The buffer is a named field so that exec.Cmd's io.Copy cannot reach
// bytes.Buffer.ReadFrom and bypass the cap.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		return len(p), nil
	}

	if _, err := b.buf.Write(p[:min(room, len(p))]); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (b *limitedBuffer) Len() int {
	return b.buf.Len()
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}
