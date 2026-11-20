package pipeline

import (
	"context"
	"sync"
)

// admission counts running requests. Closing it under the same lock that
// guards Add means no request can start once a drain has begun waiting.
type admission struct {
	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
}

func (a *admission) enter() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return false
	}

	a.active.Add(1)
	return true
}

func (a *admission) leave() {
	a.active.Done()
}

func (a *admission) close() <-chan struct{} {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()

	idle := make(chan struct{})
	go func() {
		a.active.Wait()
		close(idle)
	}()

	return idle
}

// Enter admits a request unless a drain has begun. Every true result needs
// a matching Leave; a false result means the caller sends ErrShuttingDown.
func (p *Pipeline) Enter() bool {
	return p.admission.enter()
}

func (p *Pipeline) Leave() {
	p.admission.leave()
}

// Drain stops admitting requests and waits until every admitted request has
// left, or ctx ends.
func (p *Pipeline) Drain(ctx context.Context) error {
	select {
	case <-p.admission.close():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
