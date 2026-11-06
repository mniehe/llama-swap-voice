// Package fakeffmpeg turns a Go test binary into a stand-in for ffmpeg so
// tests can observe the arguments, stdin EOF and SIGTERM without a real
// ffmpeg. TestMain must call RunIfRequested before m.Run.
package fakeffmpeg

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	recordEnv = "LLAMA_SWAP_VOICE_FAKE_FFMPEG_RECORD"
	failEnv   = "LLAMA_SWAP_VOICE_FAKE_FFMPEG_FAIL"

	EventArgs    = "ARGS"
	EventEOF     = "EOF"
	EventSIGTERM = "SIGTERM"

	stdinPipe   = "pipe:0"
	exitSIGTERM = 143
	pollEvery   = 10 * time.Millisecond
)

type Fake struct {
	Path   string
	record string
}

func Install(t *testing.T) *Fake {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	record := t.TempDir() + "/ffmpeg.log"
	t.Setenv(recordEnv, record)

	return &Fake{Path: exe, record: record}
}

func (f *Fake) FailWith(t *testing.T, message string) {
	t.Setenv(failEnv, message)
}

func (f *Fake) Events() []string {
	data, err := os.ReadFile(f.record)
	if err != nil {
		return nil
	}

	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func (f *Fake) Args() []string {
	for _, event := range f.Events() {
		if args, ok := strings.CutPrefix(event, EventArgs+" "); ok {
			return strings.Fields(args)
		}
	}

	return nil
}

func (f *Fake) WaitFor(timeout time.Duration, events ...string) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, event := range f.Events() {
			for _, want := range events {
				if event == want {
					return event, true
				}
			}
		}
		time.Sleep(pollEvery)
	}

	return "", false
}

func RunIfRequested() {
	record := os.Getenv(recordEnv)
	if record == "" {
		return
	}

	os.Exit(run(record))
}

func run(record string) int {
	appendEvent(record, EventArgs+" "+strings.Join(os.Args[1:], " "))

	if message := os.Getenv(failEnv); message != "" {
		fmt.Fprintln(os.Stderr, message)
		return 1
	}

	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	go func() {
		<-terminated
		appendEvent(record, EventSIGTERM)
		os.Exit(exitSIGTERM)
	}()

	input, err := openInput(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer input.Close()

	if _, err := io.Copy(os.Stdout, input); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	appendEvent(record, EventEOF)
	return 0
}

func openInput(args []string) (*os.File, error) {
	index := slices.Index(args, "-i")
	if index < 0 || index+1 >= len(args) || args[index+1] == stdinPipe {
		return os.Stdin, nil
	}

	return os.Open(args[index+1])
}

func appendEvent(record, event string) {
	file, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()

	fmt.Fprintln(file, event)
}
