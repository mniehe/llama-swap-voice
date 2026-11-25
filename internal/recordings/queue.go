package recordings

import (
	"context"
	"log/slog"
	"time"
)

const (
	// maxQueuedBytes bounds what waits to be saved. It holds one upload at
	// the 20-minute transcription limit, about 38 MB, with a transcript at
	// the upstream's 1 MiB bound, and room to spare.
	maxQueuedBytes = 64 << 20
	// queuedItemOverhead is charged for each queued recording besides its
	// WAV and transcript: the item, its strings' headers and the slice slot,
	// rounded up, so that many tiny recordings cannot queue for free.
	queuedItemOverhead = 512
)

// queued is one recording waiting for the worker. at is when Record was
// called, and charge what the queue counted for it.
type queued struct {
	wav        []byte
	source     string
	transcript string
	requestID  string
	at         time.Time
	charge     int64
}

// Record queues wav for the worker to save, and owns it from then on. The
// recording is dated now, not when the worker saves it. Record never waits
// on the disk: when the queue would pass maxQueuedBytes it drops the
// recording and logs a warning.
func (s *Store) Record(wav []byte, source, transcript, requestID string) {
	item := queued{
		wav:        wav,
		source:     source,
		transcript: transcript,
		requestID:  requestID,
		at:         s.now(),
		charge:     int64(len(wav)+len(transcript)) + queuedItemOverhead,
	}

	s.queueMu.Lock()
	if s.queuedBytes+item.charge > s.maxQueued {
		s.queueMu.Unlock()
		s.log.Warn("recording dropped: the save queue is full",
			slog.String("request_id", requestID), slog.String("source", source), slog.Int64("bytes", item.charge))
		return
	}
	s.queue = append(s.queue, item)
	s.queuedBytes += item.charge
	s.queueMu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run saves queued recordings in order and collects every interval until ctx
// ends; a collection that falls due while the queue is busy runs between two
// saves. Run then keeps saving what is queued until the queue is empty or
// drain ends, and drops the rest with one warning.
func (s *Store) Run(ctx, drain context.Context) {
	ticks, stop := s.ticks()
	defer stop()

	for {
		s.saveQueued(ctx, ticks)

		select {
		case <-ctx.Done():
			s.drain(drain)
			return
		case <-s.wake:
		case <-ticks:
			s.collect()
		}
	}
}

// saveQueued saves recordings until the queue is empty or ctx ends, and
// collects after any save during which a tick arrived. A nil ticks never
// collects.
func (s *Store) saveQueued(ctx context.Context, ticks <-chan time.Time) {
	for ctx.Err() == nil {
		item, ok := s.next()
		if !ok {
			return
		}
		s.saveItem(&item)

		select {
		case <-ticks:
			s.collect()
		default:
		}
	}
}

func (s *Store) collect() {
	if err := s.Collect(); err != nil {
		s.log.Warn("could not collect expired recordings", slog.Any("error", err))
	}
}

func (s *Store) drain(ctx context.Context) {
	s.saveQueued(ctx, nil)

	if dropped := s.dropQueued(); dropped > 0 {
		s.log.Warn("recordings dropped at shutdown", slog.Int("count", dropped))
	}
}

func (s *Store) saveItem(item *queued) {
	if err := s.saveAt(item.at, item.wav, item.source, item.transcript); err != nil {
		s.log.Warn("could not save recording",
			slog.String("request_id", item.requestID), slog.String("source", item.source), slog.Any("error", err))
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
	s.queuedBytes -= item.charge

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
