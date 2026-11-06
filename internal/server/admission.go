package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

var errShuttingDown = errors.New("server is shutting down")

// admission counts running handlers. Closing it under the same lock that
// guards Add means no handler can start once a drain has begun waiting.
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

func (s *Server) admitted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.admission.enter() {
			s.reject(w, errShuttingDown)
			return
		}
		defer s.admission.leave()

		next.ServeHTTP(w, r)
	})
}

// Drain stops admitting requests and waits until every admitted handler has
// returned, or ctx ends.
func (s *Server) Drain(ctx context.Context) error {
	select {
	case <-s.admission.close():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
