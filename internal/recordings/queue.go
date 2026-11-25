package recordings

import (
	"context"
	"log/slog"
)

// maxQueuedBytes bounds the WAV waiting to be saved. It holds one upload at
// the 20-minute transcription limit, about 38 MB, with room to spare.
const maxQueuedBytes = 64 << 20

// queued is one recording waiting for the worker.
type queued struct {
	wav        []byte
	source     string
	transcript string
}

// Record queues wav for the worker to save, and owns it from then on. It
// never waits on the disk: when the queue would pass maxQueuedBytes it drops
// the recording and logs a warning.
func (s *Store) Record(wav []byte, source, transcript string) {
	size := int64(len(wav))

	s.queueMu.Lock()
	if s.queuedBytes+size > s.maxQueued {
		s.queueMu.Unlock()
		s.log.Warn("recording dropped: the save queue is full", slog.String("source", source), slog.Int64("bytes", size))
		return
	}
	s.queue = append(s.queue, queued{wav: wav, source: source, transcript: transcript})
	s.queuedBytes += size
	s.queueMu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run saves queued recordings in order and collects every interval until ctx
// ends. It then keeps saving what is queued until the queue is empty or
// drain ends, and drops the rest with one warning.
func (s *Store) Run(ctx, drain context.Context) {
	ticks, stop := s.ticks()
	defer stop()

	for {
		s.saveQueued(ctx)

		select {
		case <-ctx.Done():
			s.drain(drain)
			return
		case <-s.wake:
		case <-ticks:
			if err := s.Collect(); err != nil {
				s.log.Warn("could not collect expired recordings", slog.Any("error", err))
			}
		}
	}
}

// saveQueued saves recordings until the queue is empty or ctx ends.
func (s *Store) saveQueued(ctx context.Context) {
	for ctx.Err() == nil {
		item, ok := s.next()
		if !ok {
			return
		}
		s.saveItem(&item)
	}
}

func (s *Store) drain(ctx context.Context) {
	s.saveQueued(ctx)

	if dropped := s.dropQueued(); dropped > 0 {
		s.log.Warn("recordings dropped at shutdown", slog.Int("count", dropped))
	}
}

func (s *Store) saveItem(item *queued) {
	if err := s.Save(item.wav, item.source, item.transcript); err != nil {
		s.log.Warn("could not save recording", slog.String("source", item.source), slog.Any("error", err))
	}
}

// next takes the oldest queued recording.
func (s *Store) next() (queued, bool) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	if len(s.queue) == 0 {
		return queued{}, false
	}

	item := s.queue[0]
	s.queue[0] = queued{}
	s.queue = s.queue[1:]
	s.queuedBytes -= int64(len(item.wav))

	return item, true
}

// dropQueued empties the queue and returns how many recordings it held.
func (s *Store) dropQueued() int {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	dropped := len(s.queue)
	s.queue = nil
	s.queuedBytes = 0

	return dropped
}
