package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	speechFixture    = "speech.wav"
	speechRate       = 24000
	pausedAudioBytes = speechRate / 2 * bytesPerSample
	streamTimeout    = 3 * time.Second

	stretchedSpeed           = 1.5
	stretchedLengthTolerance = 0.02

	// More than the Opus header pages, so the test needs encoded audio, not
	// just headers, to leave while the upstream is paused.
	earlyBytes       = 512
	disconnectRounds = 100

	// The fixture is cut where it is mid-word, so its last 44 ms is speech.
	speechTailSamples = 33600
	tailWindowSamples = speechRate / 100
	minSpeechTailDB   = -30.0
	tailToleranceDB   = 0.3
	maxCodecLag       = speechRate / 10
	alignmentSamples  = speechRate / 2

	// shine adds 1.02 dB on top of the MP3 encoder's -2 dB input scale.
	mp3NetGainDB = -0.98

	// With the encoder's lookahead, this many samples fill exactly two Ogg
	// pages of Opus packets.
	wholePagesSamples = 4200

	riffSizeOffset = 4
	dataSizeOffset = wavHeaderBytes - 4
	unknownSize    = math.MaxUint32
	fullScale      = 1 << 15
)

// streamedSpeech is the speech fixture with the unknown RIFF and data sizes
// llama-server streams.
func streamedSpeech(t *testing.T, samples int) []byte {
	t.Helper()

	fixture := readFixture(t, speechFixture)
	wav := bytes.Clone(fixture[:wavHeaderBytes+samples*bytesPerSample])
	binary.LittleEndian.PutUint32(wav[riffSizeOffset:], unknownSize)
	binary.LittleEndian.PutUint32(wav[dataSizeOffset:], unknownSize)

	return wav
}

func fixtureSamples(t *testing.T) int {
	return (len(readFixture(t, speechFixture)) - wavHeaderBytes) / bytesPerSample
}

func serveBody(t *testing.T, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, string(body))
	}
}

// servePaused sends the header and the first half second of audio, then
// holds the body open until release is closed.
func servePaused(t *testing.T, wav []byte, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, string(wav[:wavHeaderBytes+pausedAudioBytes]))
		assert.NoError(t, http.NewResponseController(w).Flush())

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeOrFail(t, w, string(wav[wavHeaderBytes+pausedAudioBytes:]))
	}
}

type earlyReply struct {
	resp  *http.Response
	early []byte
	err   error
}

func requestEarlyBytes(t *testing.T, h *harness, body map[string]any) <-chan earlyReply {
	payload, err := json.Marshal(body)
	require.NoError(t, err)

	got := make(chan earlyReply, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+speechPath, bytes.NewReader(payload))
		if err != nil {
			got <- earlyReply{err: err}
			return
		}
		req.Header.Set("Content-Type", contentTypeJSON)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- earlyReply{err: err}
			return
		}

		early := make([]byte, earlyBytes)
		_, err = io.ReadFull(resp.Body, early)
		got <- earlyReply{resp: resp, early: early, err: err}
	}()

	return got
}

// requireStreamedWhilePaused returns the response and its whole body once
// its first bytes have arrived before the upstream was released.
func requireStreamedWhilePaused(t *testing.T, h *harness, release chan struct{}, request map[string]any) (resp *http.Response, body []byte) {
	t.Helper()

	select {
	case got := <-requestEarlyBytes(t, h, request):
		close(release)
		require.NoError(t, got.err)
		t.Cleanup(func() { closeQuietly(t, got.resp.Body) })
		require.Equal(t, http.StatusOK, got.resp.StatusCode)
		return got.resp, append(got.early, readBody(t, got.resp)...)
	case <-time.After(streamTimeout):
		close(release)
		require.FailNow(t, "the reply did not start while the upstream held its body open")
		return nil, nil
	}
}

func TestServer_HandleSpeech_StreamsWhileUpstreamPaused(t *testing.T) {
	wav := streamedSpeech(t, fixtureSamples(t))

	for _, format := range []string{"wav", "pcm", "mp3", "opus"} {
		t.Run(format, func(t *testing.T) {
			release := make(chan struct{})
			h := newHarness(t, servePaused(t, wav, release))

			_, body := requireStreamedWhilePaused(t, h, release, map[string]any{"input": "hi", "response_format": format})

			assert.Greater(t, len(body), earlyBytes)
		})
	}
}

func TestServer_HandleSpeech_OpusAtSpeedStreamsWhileUpstreamPaused(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, servePaused(t, streamedSpeech(t, fixtureSamples(t)), release))

	resp, ogg := requireStreamedWhilePaused(t, h, release, map[string]any{"input": "hi", "response_format": "opus", "speed": 1.15})

	assert.Equal(t, contentTypeOgg, resp.Header.Get("Content-Type"))
	assert.True(t, bytes.HasPrefix(ogg, []byte("OggS")), "want an Ogg stream")
}

func TestServer_HandleSpeech_WAVAtSpeedHasStretchedLength(t *testing.T) {
	samples := fixtureSamples(t)
	h := newHarness(t, serveBody(t, streamedSpeech(t, samples)))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "wav", "speed": stretchedSpeed})
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	got := len(decodeWAV(t, body)) / bytesPerSample
	want := float64(samples) / stretchedSpeed
	t.Logf("wav at %g: %d samples from %d, want %.0f", stretchedSpeed, got, samples, want)
	assert.InDelta(t, want, float64(got), want*stretchedLengthTolerance)
}

func decodeWAV(t *testing.T, wav []byte) []byte {
	t.Helper()

	pcm, err := audio.NewWAVReader(bytes.NewReader(wav))
	require.NoError(t, err)
	data, err := io.ReadAll(pcm)
	require.NoError(t, err)

	return data
}

func requireTool(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH, so the oracle cannot run: %v", name, err)
	}

	return path
}

func runTool(t *testing.T, tool string, args ...string) string {
	t.Helper()

	output, err := exec.CommandContext(t.Context(), tool, args...).CombinedOutput()
	t.Logf("%s %s:\n%s", filepath.Base(tool), strings.Join(args, " "), output)
	require.NoError(t, err)

	return string(output)
}

func writeReply(t *testing.T, name string, body []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	return path
}

// opusdecSamples decodes at the OpusHead input rate, 24 kHz, which opusdec
// picks by default, and counts the samples left after pre-skip and end
// trimming.
func opusdecSamples(t *testing.T, ogg []byte) int {
	t.Helper()

	opusdec := requireTool(t, "opusdec")
	decoded := filepath.Join(t.TempDir(), "reply.wav")
	runTool(t, opusdec, writeReply(t, "reply.opus", ogg), decoded)

	wav, err := os.Open(decoded)
	require.NoError(t, err)
	defer closeQuietly(t, wav)

	pcm, err := audio.NewWAVReader(wav)
	require.NoError(t, err)
	data, err := io.ReadAll(pcm)
	require.NoError(t, err)

	return len(data) / bytesPerSample
}

func TestServer_HandleSpeech_OpusPassesOpusTools(t *testing.T) {
	opusinfo := requireTool(t, "opusinfo")

	for _, samples := range []int{fixtureSamples(t), wholePagesSamples} {
		t.Run(strconv.Itoa(samples), func(t *testing.T) {
			h := newHarness(t, serveBody(t, streamedSpeech(t, samples)))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "opus"})
			ogg := readBody(t, resp)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(ogg))
			assert.Equal(t, contentTypeOgg, resp.Header.Get("Content-Type"))
			reply := writeReply(t, "reply.opus", ogg)
			report, err := exec.CommandContext(t.Context(), opusinfo, reply).CombinedOutput()
			t.Logf("opusinfo:\n%s", report)
			assert.NotContains(t, string(report), "WARNING")
			require.NoError(t, err)
			assert.Contains(t, string(report), "Pre-skip: ")
			assert.Equal(t, samples, opusdecSamples(t, ogg), "decoded length differs from the upstream's")
		})
	}
}

func ffmpegSamples(t *testing.T, mp3 []byte) []int16 {
	t.Helper()

	ffmpeg := requireTool(t, "ffmpeg")
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-nostdin", "-loglevel", "error", "-i", writeReply(t, "reply.mp3", mp3),
		"-f", "s16le", "-ac", "1", "-ar", strconv.Itoa(speechRate), "pipe:1")
	raw, err := cmd.Output()
	require.NoError(t, err, "ffmpeg could not decode the MP3")

	return toSamples(t, raw)
}

func toSamples(t *testing.T, raw []byte) []int16 {
	t.Helper()

	samples := make([]int16, len(raw)/bytesPerSample)
	_, err := binary.Decode(raw, binary.LittleEndian, samples)
	require.NoError(t, err)

	return samples
}

func energyDB(samples []int16) float64 {
	var sum float64
	for _, sample := range samples {
		value := float64(sample) / fullScale
		sum += value * value
	}

	return 10 * math.Log10(sum/float64(len(samples)))
}

// codecLag finds the delay that best lines the decoded audio up with the
// input, so the test does not depend on the encoder's own figure.
func codecLag(input, decoded []int16) int {
	best, bestScore := 0, math.Inf(-1)
	for lag := 0; lag <= maxCodecLag; lag++ {
		var score float64
		for i := range min(alignmentSamples, len(input), len(decoded)-lag) {
			score += float64(input[i]) * float64(decoded[i+lag])
		}
		if score > bestScore {
			best, bestScore = lag, score
		}
	}

	return best
}

func TestServer_HandleSpeech_MP3KeepsTheFinalSpeech(t *testing.T) {
	wav := streamedSpeech(t, speechTailSamples)
	input := toSamples(t, wav[wavHeaderBytes:])
	inputTail := energyDB(input[len(input)-tailWindowSamples:])
	require.Greater(t, inputTail, minSpeechTailDB, "the input must end in speech")
	h := newHarness(t, serveBody(t, wav))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "mp3"})
	mp3 := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(mp3))

	decoded := ffmpegSamples(t, mp3)
	lag := codecLag(input, decoded)
	end := lag + len(input)
	t.Logf("mp3: %d input samples, %d decoded, lag %d", len(input), len(decoded), lag)
	require.GreaterOrEqual(t, len(decoded), end, "decoded MP3 ends before the input's last sample")
	decodedTail := energyDB(decoded[end-tailWindowSamples : end])
	t.Logf("tail energy over the last 10 ms: input %.1f dB, decoded %.1f dB", inputTail, decodedTail)
	assert.InDelta(t, inputTail+mp3NetGainDB, decodedTail, tailToleranceDB)
}

func TestServer_HandleSpeech_ClientDisconnectsMidOpusStream(t *testing.T) {
	wav := streamedSpeech(t, fixtureSamples(t))
	// A disconnect can cancel the upstream call while it is still writing, so
	// its write errors are expected rather than test failures.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		if _, err := w.Write(wav); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
	}, func(cfg *Config) { cfg.RequestTimeout = streamTimeout })

	for round := range disconnectRounds {
		ctx, cancel := context.WithCancel(t.Context())
		payload, err := json.Marshal(map[string]any{"input": "hi", "response_format": "opus"})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+speechPath, bytes.NewReader(payload))
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "round %d", round)
		_, err = io.ReadFull(resp.Body, make([]byte, len("OggS")))
		require.NoError(t, err, "round %d: no Ogg page before the disconnect", round)
		cancel()
		closeQuietly(t, resp.Body)
	}

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "wav"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a disconnected request kept its TTS slot")
}

var errNoEncoder = errors.New("no encoder for you")

// useEncoder swaps the encoder constructor before the harness starts serving.
func useEncoder(t *testing.T, build func(audio.Format, io.Writer) (io.WriteCloser, error)) {
	t.Helper()

	original := newEncoder
	newEncoder = build
	t.Cleanup(func() { newEncoder = original })
}

// panickingEncoder passes its first write through, so the 200 is on the
// wire, and then panics.
type panickingEncoder struct {
	w io.Writer
}

func (e *panickingEncoder) Write(p []byte) (int, error) {
	if _, err := e.w.Write(p); err != nil {
		return 0, err
	}

	panic(errNoEncoder)
}

func (*panickingEncoder) Close() error {
	return nil
}

// panicFirstEncoder makes the first request's encoder panic and every later
// one real.
func panicFirstEncoder(t *testing.T) {
	var built atomic.Int64
	useEncoder(t, func(format audio.Format, w io.Writer) (io.WriteCloser, error) {
		if built.Add(1) == 1 {
			return &panickingEncoder{w: w}, nil
		}

		return audio.NewEncoder(format, w)
	})
}

func speechWithin(t *testing.T, h *harness, timeout time.Duration, body map[string]any) (*http.Response, error) {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+speechPath, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentTypeJSON)

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		t.Cleanup(func() { closeQuietly(t, resp.Body) })
	}

	return resp, err
}

func TestServer_HandleSpeech_EncoderPanicReleasesUpstream(t *testing.T) {
	for _, speed := range []float64{normalSpeed, 1.5} {
		t.Run(strconv.FormatFloat(speed, 'f', -1, 64), func(t *testing.T) {
			panicFirstEncoder(t)
			neverReleased := make(chan struct{})
			h := newHarness(t, servePaused(t, streamedSpeech(t, speechRate), neverReleased))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm", "speed": speed})
			require.Equal(t, http.StatusOK, resp.StatusCode)
			_, err := io.ReadAll(resp.Body)
			require.Error(t, err, "the panicking stream ended cleanly")

			next, err := speechWithin(t, h, streamTimeout, map[string]any{"input": "hi", "response_format": "pcm"})
			require.NoError(t, err, "the panicked request kept its TTS slot")
			assert.Equal(t, http.StatusOK, next.StatusCode)
		})
	}
}

func TestServer_HandleSpeech_EncoderConstructionFailureIsJSONError(t *testing.T) {
	useEncoder(t, func(audio.Format, io.Writer) (io.WriteCloser, error) {
		return nil, errNoEncoder
	})
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "opus"})

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Contains(t, errorMessage(t, resp), errNoEncoder.Error())
}
