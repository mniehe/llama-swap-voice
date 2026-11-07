package server

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
)

// ttsGate admits at most a fixed number of holders and queues the rest in
// arrival order. A release hands its slot straight to the oldest waiter, so
// a newcomer can never overtake the queue.
type ttsGate struct {
	mu      sync.Mutex
	free    int
	waiters []chan struct{}
}

func newTTSGate(size int) *ttsGate {
	return &ttsGate{free: size}
}

func (g *ttsGate) acquire(ctx context.Context) error {
	g.mu.Lock()
	if g.free > 0 && len(g.waiters) == 0 {
		g.free--
		g.mu.Unlock()
		return nil
	}

	turn := make(chan struct{})
	g.waiters = append(g.waiters, turn)
	g.mu.Unlock()

	select {
	case <-turn:
		return nil
	case <-ctx.Done():
		return g.abandon(turn, ctx.Err())
	}
}

// A waiter missing from the queue was already handed the slot by a release
// that raced its context, so it must pass the slot on.
func (g *ttsGate) abandon(turn chan struct{}, err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if i := slices.Index(g.waiters, turn); i >= 0 {
		g.waiters = slices.Delete(g.waiters, i, i+1)
		return err
	}

	g.handOver()
	return err
}

func (g *ttsGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.handOver()
}

func (g *ttsGate) handOver() {
	if len(g.waiters) == 0 {
		g.free++
		return
	}

	close(g.waiters[0])
	g.waiters = slices.Delete(g.waiters, 0, 1)
}

// gatedBody gives the gate slot back once the upstream body is drained or
// closed, whichever comes first.
type gatedBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(b.release)
	}

	return n, err
}

func (b *gatedBody) Close() error {
	b.once.Do(b.release)
	return b.ReadCloser.Close()
}
