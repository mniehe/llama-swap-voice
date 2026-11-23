package wyoming

import (
	"context"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const acceptRetryWarning = "wyoming accept failed, retrying"

// flakyListener answers each Accept with the next scripted error; a nil
// entry, or an empty script, accepts from the pipe. It records when each
// call began. Once closed it reports net.ErrClosed, as a real listener does.
type flakyListener struct {
	*pipeListener
	mu     sync.Mutex
	script []error
	calls  []time.Time
}

func newFlakyListener(script ...error) *flakyListener {
	return &flakyListener{pipeListener: newPipeListener(), script: script}
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	var next error
	if len(f.script) > 0 {
		next, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	select {
	case <-f.closed:
		return nil, net.ErrClosed
	default:
	}
	if next != nil {
		return nil, next
	}

	return f.pipeListener.Accept()
}

// gaps returns the time between the starts of consecutive Accept calls.
func (f *flakyListener) gaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	gaps := make([]time.Duration, 0, len(f.calls))
	for i := 1; i < len(f.calls); i++ {
		gaps = append(gaps, f.calls[i].Sub(f.calls[i-1]))
	}

	return gaps
}

func (f *flakyListener) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

// acceptFailure is the error a TCP listener's Accept returns for errno.
func acceptFailure(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

func repeated(err error, n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}

	return errs
}

func (f *flakyListener) serve(s *Server) <-chan error {
	served := make(chan error, 1)
	go func() { served <- s.Serve(f) }()

	return served
}

func TestServe_TemporaryAcceptError_KeepsServing(t *testing.T) {
	retried := map[string]syscall.Errno{
		"EMFILE":  syscall.EMFILE,
		"ENFILE":  syscall.ENFILE,
		"ENOBUFS": syscall.ENOBUFS,
		"ENOMEM":  syscall.ENOMEM,
	}

	for name, errno := range retried {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newBubbleHarness(t)
				listener := newFlakyListener(acceptFailure(errno))
				served := listener.serve(h.server)
				synctest.Wait()
				requireNotReturned(t, served, "Serve after a retried accept error")

				c := listener.dialClient(t)
				c.send(typeDescribe, nil, nil)
				c.readType(typeInfo)

				warnings := h.logs.lines(t, acceptRetryWarning)
				require.Len(t, warnings, 1)
				assert.Equal(t, "WARN", warnings[0]["level"])
				assert.Contains(t, warnings[0]["error"], errno.Error())
				require.NoError(t, h.server.Close())
				require.NoError(t, <-served)
			})
		})
	}
}

// Ten failures double the wait from 5 ms to the 1 s cap. A connection then
// accepted resets it, so the next failure waits 5 ms again.
func TestServe_AcceptRetry_BackoffDoublesToTheCapAndResets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		emfile := acceptFailure(syscall.EMFILE)
		script := append(repeated(emfile, 10), nil, emfile)
		h := newBubbleHarness(t)
		listener := newFlakyListener(script...)
		served := listener.serve(h.server)

		listener.dialClient(t)
		listener.dialClient(t)
		synctest.Wait()

		ms := time.Millisecond
		assert.Equal(t, []time.Duration{
			5 * ms, 10 * ms, 20 * ms, 40 * ms, 80 * ms, 160 * ms, 320 * ms, 640 * ms, time.Second, time.Second,
			0, 5 * ms, 0,
		}, listener.gaps(), "the last gap leads to the Accept waiting for a third connection")
		require.NoError(t, h.server.Close())
		require.NoError(t, <-served)
	})
}

func TestServe_StopDuringAcceptBackoff_ReturnsNilAtOnce(t *testing.T) {
	stops := map[string]func(*Server) error{
		"Close":    (*Server).Close,
		"Shutdown": func(s *Server) error { return s.Shutdown(context.Background()) },
	}

	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newBubbleHarness(t)
				listener := newFlakyListener(repeated(acceptFailure(syscall.EMFILE), 3)...)
				served := listener.serve(h.server)
				synctest.Wait()
				start := time.Now()

				require.NoError(t, stop(h.server))

				require.NoError(t, <-served)
				assert.Zero(t, time.Since(start), "Serve waited out its backoff")
				assert.Equal(t, 2, listener.callCount(), "the failed accept, then the closed listener")
			})
		})
	}
}

// closeRacingListener holds each Accept until the listener is closed and
// then fails it with err, as a real Accept can return EMFILE just as Close
// or Shutdown begins.
type closeRacingListener struct {
	*pipeListener
	err error
}

func (l *closeRacingListener) Accept() (net.Conn, error) {
	<-l.closed

	return nil, l.err
}

func TestServe_RetriedAcceptErrorAfterStop_ReturnsNil(t *testing.T) {
	stops := map[string]func(*Server) error{
		"Close":    (*Server).Close,
		"Shutdown": func(s *Server) error { return s.Shutdown(context.Background()) },
	}

	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newBubbleHarness(t)
				listener := &closeRacingListener{pipeListener: newPipeListener(), err: acceptFailure(syscall.EMFILE)}
				served := make(chan error, 1)
				go func() { served <- h.server.Serve(listener) }()
				synctest.Wait()
				requireNotReturned(t, served, "Serve before the stop")

				require.NoError(t, stop(h.server))

				require.NoError(t, <-served, "an EMFILE that raced the stop is not an accept failure")
				assert.Empty(t, h.logs.lines(t, acceptRetryWarning))
			})
		})
	}
}

func TestServe_UnlistedAcceptErrno_IsReturnedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		listener := newFlakyListener(acceptFailure(syscall.EINVAL))
		served := listener.serve(h.server)
		synctest.Wait()

		select {
		case err := <-served:
			require.ErrorIs(t, err, syscall.EINVAL)
		default:
			require.NoError(t, h.server.Close())
			require.FailNow(t, "Serve retried an unlisted accept error", "error: %v", <-served)
		}
		assert.Equal(t, 1, listener.callCount())
		assert.Empty(t, h.logs.lines(t, acceptRetryWarning))
	})
}
